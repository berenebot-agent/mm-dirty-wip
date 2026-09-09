package store_test

import (
	"context"
	"sync"
	"testing"
)

func TestListAPIKeysIncludesRoleless(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	if _, _, err := s.CreateAPIKey(ctx, u.AccountID, "roleless", false, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateAPIKey(ctx, u.AccountID, "multi", false, map[string]string{b[0].ID: "owner", b[1].ID: "read"}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ListAPIKeys(ctx, u.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]int{}
	for _, k := range keys {
		roles[k.Name] = len(k.Roles)
	}
	if _, ok := roles["roleless"]; !ok {
		t.Fatalf("roleless key missing from %#v", roles)
	}
	if roles["roleless"] != 0 {
		t.Fatalf("roleless key has roles %#v", roles)
	}
	if roles["multi"] != 2 {
		t.Fatalf("multi key roles %#v", roles)
	}
}

func TestListAPIKeysConcurrentCompletes(t *testing.T) {
	ctx := context.Background()
	s, u, _, b := testStore(t)
	for i := 0; i < 4; i++ {
		if _, _, err := s.CreateAPIKey(ctx, u.AccountID, "k", false, map[string]string{b[0].ID: "owner"}); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.ListAPIKeys(ctx, u.AccountID); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent store.ListAPIKeys: %v", err)
	}
}
