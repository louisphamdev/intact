package httpapi

import (
	"strings"
	"testing"
)

// Mobile keyboards, SMS autofill and password managers insert the whole code into
// one field through the input event, with no paste event. One real field is what
// keeps that working; six maxlength=1 boxes cut it to the first digit.
func TestLoginPageHasOneCodeField(t *testing.T) {
	page := renderLogin("")
	for want, n := range map[string]int{
		`autocomplete="one-time-code"`: 1,
		`name="totp"`:                  1,
		`inputmode="numeric"`:          1,
		`maxlength="1"`:                0,
	} {
		if got := strings.Count(page, want); got != n {
			t.Errorf("%s appears %d times, want %d", want, got, n)
		}
	}
}

func TestLoginPageErrorMarksCodeRow(t *testing.T) {
	if page := renderLogin("Wrong or expired code. Try again."); !strings.Contains(page, `class="otp err" id="otp"`) {
		t.Fatal("the error page does not mark the code row")
	}
}
