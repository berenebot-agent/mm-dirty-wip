package auth

// Unexported access is necessary to saturate the process-wide gate without
// running production-cost KDFs. Public password tests live in tests/unit/auth.
import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dellarb/mailmoose/tests/support/smoke"
)

func TestArgonAdmissionAllPathsBounded(t *testing.T) {
	// Serial: this test occupies all slots of the process-global admission gate.
	smoke.Require(t)
	for i := 0; i < cap(argonAdmission); i++ {
		argonAdmission <- struct{}{}
	}
	t.Cleanup(func() {
		for i := 0; i < cap(argonAdmission); i++ {
			releaseArgonSlot()
		}
	})
	results := make(chan bool, 3)
	start := time.Now()
	go func() { results <- !CheckPassword("$argon2id$invalid", "password") }()
	go func() {
		_, err := HashPassword("long enough password")
		results <- errors.Is(err, ErrArgonBusy)
	}()
	go func() {
		_, err := CheckPasswordContext(context.Background(), "$argon2id$invalid", "password", 0)
		results <- errors.Is(err, ErrArgonBusy)
	}()
	timer := time.NewTimer(defaultArgonAdmissionTimeout + 3*time.Second)
	defer timer.Stop()
	for i := 0; i < 3; i++ {
		select {
		case ok := <-results:
			if !ok {
				t.Fatal("saturated admission did not fail closed")
			}
		case <-timer.C:
			t.Fatal("password path waited indefinitely on admission")
		}
	}
	if time.Since(start) < defaultArgonAdmissionTimeout {
		t.Fatal("test did not exercise bounded default wait")
	}
}
