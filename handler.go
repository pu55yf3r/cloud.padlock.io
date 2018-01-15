package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	pc "github.com/maklesoft/padlock-cloud/padlockcloud"
	"github.com/stripe/stripe-go"
	"github.com/stripe/stripe-go/customer"
	"github.com/stripe/stripe-go/sub"
	"io/ioutil"
	"net/http"
)

type Dashboard struct {
	*Server
}

func (h *Dashboard) Handle(w http.ResponseWriter, r *http.Request, auth *pc.AuthToken) error {
	acc := auth.Account()
	subAcc, err := h.AccountFromEmail(acc.Email, true)
	if err != nil {
		return err
	}

	accMap := acc.ToMap()
	accMap["trackingID"] = subAcc.TrackingID

	if sub, err := EnsureSubscription(subAcc, h.Storage); err != nil {
		return err
	} else {
		accMap["subscription"] = map[string]interface{}{
			"plan":     sub.Plan,
			"status":   sub.Status,
			"trialEnd": sub.TrialEnd,
		}
	}

	if len(subAcc.Customer.Sources.Values) != 0 {
		source := subAcc.Customer.Sources.Values[0]
		accMap["paymentSource"] = map[string]string{
			"brand":    string(source.Card.Brand),
			"lastFour": source.Card.LastFour,
		}
	}

	accMap["displaySubscription"] = !NoSubRequired(auth)

	params := pc.DashboardParams(r, auth)
	params["account"] = accMap

	params["stripePublicKey"] = h.StripeConfig.PublicKey
	params["mixpanelToken"] = h.MixpanelConfig.Token

	ref := r.URL.Query().Get("ref")
	if ref == "" && params["action"] != "" {
		ref = fmt.Sprintf("action: %s", params["action"])
	}
	params["ref"] = ref

	var b bytes.Buffer
	if err := h.Templates.Dashboard.Execute(&b, params); err != nil {
		return err
	}

	b.WriteTo(w)

	go h.Track(&TrackingEvent{
		TrackingID: r.URL.Query().Get("tid"),
		Name:       "Open Dashboard",
		Properties: map[string]interface{}{
			"Action": params["action"],
			"Source": sourceFromRef(ref),
		},
	}, r, auth)

	return nil
}

type Subscribe struct {
	*Server
}

func wrapCardError(err error) error {
	// For now, card errors are the only errors we are expecting from stripe. Any other
	// errors we treat as unexpected errors (i.e. ServerError)
	if stripeErr, ok := err.(*stripe.Error); ok && stripeErr.Type == stripe.ErrorTypeCard {
		return &StripeError{stripeErr}
	} else {
		return err
	}
}

func (h *Subscribe) Handle(w http.ResponseWriter, r *http.Request, a *pc.AuthToken) error {
	if a == nil {
		return &pc.InvalidAuthToken{}
	}

	token := r.PostFormValue("stripeToken")

	if token == "" {
		return &pc.BadRequest{"No stripe token provided"}
	}

	acc, err := h.AccountFromEmail(a.Account().Email, true)
	if err != nil {
		return err
	}

	newSubscription := !acc.HasActiveSubscription()

	if err := acc.SetPaymentSource(token); err != nil {
		return wrapCardError(err)
	}

	s, err := EnsureSubscription(acc, h.Storage)
	if err != nil {
		return err
	}

	if s_, err := sub.Update(s.ID, &stripe.SubParams{
		TrialEndNow: true,
	}); err != nil {
		return wrapCardError(err)
	} else {
		*s = *s_
	}

	if err := h.Storage.Put(acc); err != nil {
		return err
	}

	var eventName string
	var action string
	if newSubscription {
		eventName = "Buy Subscription"
		action = "subscribed"
	} else {
		eventName = "Update Payment Method"
		action = "payment-updated"
	}

	http.Redirect(w, r, "/dashboard/?action="+action, http.StatusFound)

	h.Info.Printf("%s - subcribe - %s\n", pc.FormatRequest(r), acc.Email)

	go h.Track(&TrackingEvent{
		Name: eventName,
		Properties: map[string]interface{}{
			"Plan":   s.Plan.ID,
			"Source": sourceFromRef(r.URL.Query().Get("ref")),
		},
	}, r, a)

	return nil
}

type Unsubscribe struct {
	*Server
}

func (h *Unsubscribe) Handle(w http.ResponseWriter, r *http.Request, a *pc.AuthToken) error {
	acc, err := h.AccountFromEmail(a.Account().Email, true)
	if err != nil {
		return err
	}

	s := acc.Subscription()

	if s == nil {
		return &pc.BadRequest{"This account does not have an active subscription"}
	}

	if s_, err := sub.Cancel(s.ID, nil); err != nil {
		return err
	} else {
		*s = *s_
	}

	if err := h.Storage.Put(acc); err != nil {
		return err
	}

	http.Redirect(w, r, "/dashboard/?action=unsubscribed", http.StatusFound)

	h.Info.Printf("%s - unsubscribe - %s\n", pc.FormatRequest(r), acc.Email)

	go h.Track(&TrackingEvent{
		Name: "Cancel Subscription",
	}, r, a)

	return nil
}

type StripeHook struct {
	*Server
}

func (h *StripeHook) Handle(w http.ResponseWriter, r *http.Request, a *pc.AuthToken) error {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}
	event := &stripe.Event{}
	if err := json.Unmarshal(body, event); err != nil {
		return err
	}

	var c *stripe.Customer

	switch event.Type {
	case "customer.created", "customer.updated":
		c = &stripe.Customer{}
		if err := json.Unmarshal(event.Data.Raw, c); err != nil {
			return err
		}

	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		var err error
		if c, err = customer.Get(event.GetObjValue("customer"), nil); err != nil {
			return err
		}
	}

	if c != nil {
		acc, err := h.AccountFromEmail(c.Email, true)
		if err != nil {
			return err
		}

		// Only update customer if the ids match (even though that theoretically shouldn't happen,
		// it's possible that there are two stripe customers with the same email. In that case, this guard
		// against unexpected behaviour by making sure only one of the customers is used)
		if acc.Customer.ID == c.ID {
			acc.Customer = c
		}

		if err := h.Storage.Put(acc); err != nil {
			return err
		}

		h.Info.Printf("%s - stripe_hook - %s:%s", pc.FormatRequest(r), acc.Email, event.Type)
	}

	return nil
}

type Track struct {
	*Server
}

func (h *Track) Handle(w http.ResponseWriter, r *http.Request, a *pc.AuthToken) error {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return err
	}
	event := &TrackingEvent{}
	if err := json.Unmarshal(body, event); err != nil {
		return err
	}

	if err := h.Track(event, r, a); err != nil {
		return err
	}

	var response []byte
	if response, err = json.Marshal(event); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(response)

	return nil
}
