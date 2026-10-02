package chatgptauth

import (
	"context"
	"testing"
)

// TestLogout (plan 033 §3.10, A20's last step): sign-out revokes the refresh
// token at the discovery document's revocation endpoint — the token, its
// hint and the issued client id — deletes chatgpt.json, and keeps the
// registration and the host id; the refresh token no longer refreshes. A
// revocation the server refuses is "not confirmed" and the file goes all the
// same. Signed out already, it does nothing and asks nothing (the control).
func TestLogout(t *testing.T) {
	f := newFake(t)
	useFake(t, f)
	dir := nativeDir(t)
	if res, err := Logout(context.Background(), dir); err != nil || res.SignedIn {
		t.Fatalf("never signed in: %+v, %v", res, err)
	}
	signIn(t, f, dir)
	rec := readRec(t, dir)
	host, err := readHostID(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Logout(context.Background(), dir)
	if err != nil || !res.SignedIn || !res.Revoked {
		t.Fatalf("logout = %+v, %v", res, err)
	}
	f.mu.Lock()
	form := f.revokes[0]
	f.mu.Unlock()
	if form.Get("token") != rec.RefreshToken || form.Get("token_type_hint") != "refresh_token" || form.Get("client_id") != testClient {
		t.Fatal("the revocation's form is not the refresh token, its hint and the client id")
	}
	if f.refreshState(rec.RefreshToken) != "revoked" || !tokenFileGone(t, dir) {
		t.Fatal("the refresh token is not revoked, or the token file is left")
	}
	if c, _ := ReadClient(dir); c.ClientID != testClient || c.Email != testEmail {
		t.Fatal("the registration went")
	}
	if again, _ := readHostID(dir); again != host {
		t.Fatal("the host id changed")
	}
	if res, err := Logout(context.Background(), dir); err != nil || res.SignedIn {
		t.Fatalf("a second logout: %+v, %v", res, err)
	}
	if _, _, revokes, _ := f.counts(); revokes != 1 {
		t.Fatalf("revocations = %d, want 1", revokes)
	}

	signIn(t, f, dir)
	f.revokeStatus = 503
	res, err = Logout(context.Background(), dir)
	if err != nil || !res.SignedIn || res.Revoked {
		t.Fatalf("logout with the revocation refused = %+v, %v; want not confirmed", res, err)
	}
	if !tokenFileGone(t, dir) {
		t.Fatal("an unconfirmed revocation left the token file")
	}
}
