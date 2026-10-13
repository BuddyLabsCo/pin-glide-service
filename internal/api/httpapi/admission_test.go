package httpapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/BuddyLabsCo/pin-glide-service/internal/api/httpapi"
)

// Shutdown must stop new work, let admitted work finish, and stay bounded when
// admitted work never finishes.
func TestAdmissionStopsNewWorkAndWaitsForAdmittedWork(t *testing.T) {
	admission := httpapi.NewAdmission()

	release, ok := admission.Begin()
	if !ok {
		t.Fatal("open gate rejected work")
	}

	admission.Close()
	if _, ok := admission.Begin(); ok {
		t.Fatal("closed gate admitted new work")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := admission.Wait(ctx); err == nil {
		t.Fatal("Wait ignored work still in flight")
	}

	release()
	if err := admission.Wait(context.Background()); err != nil {
		t.Fatalf("Wait after release: %v", err)
	}
}
