// SPDX-License-Identifier: MIT

package litellm

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ackstorm/alitellm-operator/internal/litellm/mock"
)

// Fixture key values: `sk-` plus 16 characters, the shortest LiteLLM accepts.
const (
	wireKeyValue  = "sk-aaaaaaaaaaaaaaaa"
	otherKeyValue = "sk-bbbbbbbbbbbbbbbb"
)

func TestHashKey(t *testing.T) {
	const want = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	if got := HashKey("abc"); got != want {
		t.Fatalf("HashKey(abc) = %s, want %s", got, want)
	}
}

func TestKeyCalls_WireShape(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.RequestURI()+" "+string(b))
		switch r.URL.Path {
		case "/key/info":
			_, _ = w.Write([]byte(`{"key":"h1","info":{"key_alias":"svc","models":["m1"],"max_budget":5,"budget_duration":"7d","rpm_limit":60,"tpm_limit":null,"metadata":{"managed-by":"alitellm-operator"}}}`))
		case "/key/list":
			_, _ = w.Write([]byte(`{"keys":["h1","h2"],"total_count":2}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv.URL)
	ctx := context.Background()

	if err := c.GenerateKey(ctx, map[string]any{"key": wireKeyValue}); err != nil {
		t.Fatal(err)
	}
	info, err := c.GetKeyInfo(ctx, "h1")
	if err != nil || info.KeyAlias != "svc" || len(info.Models) != 1 || *info.MaxBudget != 5 ||
		*info.BudgetDuration != "7d" || *info.RPMLimit != 60 || info.TPMLimit != nil ||
		info.Metadata["managed-by"] != "alitellm-operator" {
		t.Fatalf("info %+v, %v", info, err)
	}
	if err := c.UpdateKey(ctx, "h1", map[string]any{"max_budget": nil}); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteKeys(ctx, []string{"h1"}); err != nil {
		t.Fatal(err)
	}
	hashes, err := c.ListKeyHashesByAlias(ctx, "a/b")
	if err != nil || len(hashes) != 2 || hashes[0] != "h1" {
		t.Fatalf("hashes %v, %v", hashes, err)
	}

	want := []string{
		`POST /key/generate {"key":"` + wireKeyValue + `"}`,
		`GET /key/info?key=h1 `,
		`POST /key/update {"key":"h1","max_budget":null}`,
		`POST /key/delete {"keys":["h1"]}`,
		`GET /key/list?key_alias=a%2Fb&size=100 `,
	}
	if len(got) != len(want) {
		t.Fatalf("calls %q", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("call %d: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestGetKeyInfo_DeletedIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"key":"h1","info":{"key_alias":"svc","status":"deleted"}}`))
	}))
	defer srv.Close()
	if _, err := newTestClient(t, srv.URL).GetKeyInfo(context.Background(), "h1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// One pass over the mock's /key/* routes, which the controller envtests use.
func TestKeyLifecycle_AgainstMock(t *testing.T) {
	srv := mock.NewServer(t)
	c := newTestClient(t, srv.URL())
	ctx := context.Background()
	const value = "sk-lifecycle0123456789"
	hash := HashKey(value)
	body := map[string]any{"key": value, "key_alias": "svc", "models": []string{"m1"}, "max_budget": 5.0}

	if err := c.GenerateKey(ctx, body); err != nil {
		t.Fatal(err)
	}
	if err := c.GenerateKey(ctx, map[string]any{"key": "short"}); rejected(err) != 400 {
		t.Fatalf("short key: want 400, got %v", err)
	}
	if err := c.GenerateKey(ctx, map[string]any{"key": otherKeyValue, "key_alias": "svc"}); rejected(err) != 400 {
		t.Fatalf("duplicate alias: want 400, got %v", err)
	}
	if hashes, err := c.ListKeyHashesByAlias(ctx, "svc"); err != nil || len(hashes) != 1 || hashes[0] != hash {
		t.Fatalf("list %v, %v", hashes, err)
	}
	if err := c.UpdateKey(ctx, hash, map[string]any{"max_budget": nil, "models": []string{"m2"}}); err != nil {
		t.Fatal(err)
	}
	info, err := c.GetKeyInfo(ctx, hash)
	if err != nil || info.MaxBudget != nil || len(info.Models) != 1 || info.Models[0] != "m2" {
		t.Fatalf("after update %+v, %v", info, err)
	}
	if err := c.DeleteKeys(ctx, []string{hash}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetKeyInfo(ctx, hash); !IsNotFound(err) {
		t.Fatalf("deleted key: want IsNotFound, got %v", err)
	}
	if err := c.DeleteKeys(ctx, []string{hash}); !IsNotFound(err) {
		t.Fatalf("delete missing: want IsNotFound, got %v", err)
	}
	if err := c.GenerateKey(ctx, body); err != nil {
		t.Fatalf("regenerate same value: %v", err)
	}
}

func rejected(err error) int {
	var r *RejectedError
	if errors.As(err, &r) {
		return r.Status
	}
	return 0
}
