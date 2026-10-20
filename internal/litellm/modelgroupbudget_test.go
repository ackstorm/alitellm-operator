// SPDX-License-Identifier: MIT

package litellm

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

func TestGetModelGroupBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/access_group/team%2Fx/budget":
			_, _ = w.Write([]byte(`{"access_group":"team/x","spend":1.5,"budget":{"budget_id":"b1","max_budget":5,"soft_budget":null,"budget_duration":"30d","budget_reset_at":null}}`))
		case "/access_group/nobudget/budget":
			_, _ = w.Write([]byte(`{"access_group":"nobudget","spend":0,"budget":null}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"detail":{"error":"Access group 'missing' not found"}}`))
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	got, err := c.GetModelGroupBudget(context.Background(), "team/x")
	if err != nil || got == nil || got.MaxBudget == nil || *got.MaxBudget != 5 ||
		got.SoftBudget != nil || got.BudgetDuration == nil || *got.BudgetDuration != "30d" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := c.GetModelGroupBudget(context.Background(), "nobudget"); err != nil || got != nil {
		t.Fatalf("no budget: got %+v, %v", got, err)
	}
	if _, err := c.GetModelGroupBudget(context.Background(), "missing"); !IsNotFound(err) {
		t.Fatalf("missing tag: want IsNotFound, got %v", err)
	}
	if _, err := c.GetModelGroupBudget(context.Background(), ""); err == nil {
		t.Fatal("empty group must error")
	}
}

func TestPutModelGroupBudget_OmitsUnsetFields(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = r.Method + " " + r.URL.EscapedPath() + " " + string(b)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	limit := 5.0
	err := newTestClient(t, srv.URL).PutModelGroupBudget(context.Background(), "openai", &ModelGroupBudgetRequest{MaxBudget: &limit})
	if err != nil {
		t.Fatal(err)
	}
	if want := `PUT /access_group/openai/budget {"max_budget":5}`; body != want {
		t.Errorf("got %q want %q", body, want)
	}
}

func TestDeleteModelGroupBudget(t *testing.T) {
	var got string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Method + " " + r.URL.EscapedPath()
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)

	if err := c.DeleteModelGroupBudget(context.Background(), "openai"); err != nil {
		t.Fatal(err)
	}
	if want := "DELETE /access_group/openai/budget"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	status = http.StatusNotFound
	if err := c.DeleteModelGroupBudget(context.Background(), "openai"); err != nil {
		t.Errorf("404 must be nil, got %v", err)
	}
}

// TestModelGroupBudget_AgainstMock pins the mock to the measured LiteLLM
// semantics (merge, null ignored, 404 on unknown tag) via the real client.
func TestModelGroupBudget_AgainstMock(t *testing.T) {
	ms := mock.NewServer(t)
	c := newTestClient(t, ms.URL())
	ctx := context.Background()

	if _, err := c.GetModelGroupBudget(ctx, "g"); !IsNotFound(err) {
		t.Fatalf("unknown tag: want IsNotFound, got %v", err)
	}
	ms.SeedModelGroup("g")
	if b, err := c.GetModelGroupBudget(ctx, "g"); err != nil || b != nil {
		t.Fatalf("no budget: %+v, %v", b, err)
	}
	max, soft := 5.0, 4.0
	if err := c.PutModelGroupBudget(ctx, "g", &ModelGroupBudgetRequest{MaxBudget: &max, SoftBudget: &soft, BudgetDuration: "30d"}); err != nil {
		t.Fatal(err)
	}
	first, err := c.GetModelGroupBudget(ctx, "g")
	if err != nil || first == nil {
		t.Fatalf("GET after PUT: %+v, %v", first, err)
	}
	max = 6
	if err := c.PutModelGroupBudget(ctx, "g", &ModelGroupBudgetRequest{MaxBudget: &max}); err != nil {
		t.Fatal(err)
	}
	got, err := c.GetModelGroupBudget(ctx, "g")
	if err != nil || got == nil {
		t.Fatalf("GET after merge PUT: %+v, %v", got, err)
	}
	if got.BudgetID != first.BudgetID || *got.MaxBudget != 6 || *got.SoftBudget != 4 || *got.BudgetDuration != "30d" {
		t.Fatalf("merge broken: first %+v got %+v", first, got)
	}
	// An empty PUT is rejected (400 on 1.106.0) and leaves the budget as is.
	if err := c.PutModelGroupBudget(ctx, "g", &ModelGroupBudgetRequest{}); err == nil {
		t.Fatal("empty PUT accepted; LiteLLM answers 400")
	}
	if b, _ := ms.ModelGroupBudget("g"); b["max_budget"] != 6.0 {
		t.Fatalf("rejected PUT changed the budget: %v", b)
	}
	if err := c.DeleteModelGroupBudget(ctx, "g"); err != nil {
		t.Fatal(err)
	}
	if _, ok := ms.ModelGroupBudget("g"); ok {
		t.Fatal("budget survived DELETE")
	}
	ms.ResetModelGroupBudgets()
	if err := c.PutModelGroupBudget(ctx, "g", &ModelGroupBudgetRequest{MaxBudget: &max}); !IsNotFound(err) {
		t.Fatalf("after reset PUT: want IsNotFound, got %v", err)
	}
}
