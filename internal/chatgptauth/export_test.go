package chatgptauth

import "testing"

// RunEventScenarios runs every scenario of TestSignInEvents' table
// (events_test.go), each in a subtest of t with a world of its own, telling
// observe every event each run reports, and answers every fixture value the
// runs used: the tokens the fake issued, the codes it issued and the
// verifiers it was sent; each attempt's state, nonce, PKCE challenge and
// verifier, host id and login_hint; every authorization URL, and every
// redirect URL and line pasted or sent to a listener; the account's email,
// subject and client id; and the server's error_description. It is the
// sign-in log's value-free scan's way in (plan 034 A11;
// signinlog_scan_test.go), from a test package that cannot reach this one's
// unexported fake.
func RunEventScenarios(t *testing.T, observe func(Event)) []string {
	t.Helper()
	var values []string
	for _, sc := range eventScenarios {
		t.Run(sc.name, func(t *testing.T) {
			run := newScenarioRun(t, observe)
			_ = sc.run(t, run)
			run.f.mu.Lock()
			run.keep(run.f.issued...)
			run.keep(run.f.codes...)
			run.keep(run.f.verifiers...)
			run.f.mu.Unlock()
			run.keep(testSubject, testEmail, testClient, testErrorDescription)
			values = append(values, run.values...)
		})
	}
	return values
}
