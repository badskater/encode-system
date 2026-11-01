package api

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/badskater/encode-system/backend/internal/auth"
	"github.com/badskater/encode-system/backend/internal/model"
	"golang.org/x/crypto/bcrypt"
)

// mintViewerSession creates a viewer-role user and returns a session token
// for it, so tests can exercise withAnyUser's viewer path.
func mintViewerSession(t *testing.T, e *testEnv, username, password string) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.server.Store.CreateUser(ctxBg(), username, string(hash), model.RoleViewer)
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	tok, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.server.Store.CreateSession(ctxBg(), auth.HashToken(tok), u.ID, time.Now().UTC().Add(sessionTTL)); err != nil {
		t.Fatal(err)
	}
	return tok
}

// ---------- Viewer role middleware ----------

func TestViewerSessionCanGetJobsCannotPost(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	viewerTok := mintViewerSession(t, e, "viewer1", "viewer-pass-123")

	// Viewer GET /api/jobs → 200.
	r1, b1 := doJSON(t, "GET", ts.URL+"/api/jobs", viewerTok, nil)
	if r1.StatusCode != 200 {
		t.Fatalf("viewer GET jobs: %d %s", r1.StatusCode, b1)
	}

	// Viewer POST /api/jobs → 403 (withAdmin stays admin-only).
	r2, b2 := doJSON(t, "POST", ts.URL+"/api/jobs", viewerTok, map[string]any{
		"series": "S", "episode": "01", "episode_dir": "S/Ep 01",
		"script_type": "vpy", "flow_id": 1,
	})
	if r2.StatusCode != 403 {
		t.Fatalf("viewer POST jobs: want 403, got %d %s", r2.StatusCode, b2)
	}
}

func TestViewerSessionCannotGetUsers(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	viewerTok := mintViewerSession(t, e, "viewer2", "viewer-pass-456")

	// /api/users is admin-only even for GET.
	r, b := doJSON(t, "GET", ts.URL+"/api/users", viewerTok, nil)
	if r.StatusCode != 403 {
		t.Fatalf("viewer GET users: want 403, got %d %s", r.StatusCode, b)
	}
}

func TestViewerCanAccessAuthMe(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	viewerTok := mintViewerSession(t, e, "viewer3", "viewer-pass-789")

	r, b := doJSON(t, "GET", ts.URL+"/api/auth/me", viewerTok, nil)
	if r.StatusCode != 200 {
		t.Fatalf("viewer GET me: %d %s", r.StatusCode, b)
	}
	var me struct {
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(b, &me); err != nil {
		t.Fatal(err)
	}
	if me.Role != model.RoleViewer {
		t.Fatalf("me role: want viewer, got %q", me.Role)
	}
}

// ---------- User CRUD ----------

func TestListUsers(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	r, b := doJSON(t, "GET", ts.URL+"/api/users", adminTok, nil)
	if r.StatusCode != 200 {
		t.Fatalf("list users: %d %s", r.StatusCode, b)
	}
	var users []*model.User
	if err := json.Unmarshal(b, &users); err != nil {
		t.Fatal(err)
	}
	if len(users) == 0 {
		t.Fatal("expected at least the seeded admin")
	}
	// Admin must be present and must NOT carry PasswordHash.
	var admin *model.User
	for _, u := range users {
		if u.Username == "admin" {
			admin = u
		}
		if u.PasswordHash != "" {
			t.Fatalf("user %s has non-empty PasswordHash in list response", u.Username)
		}
	}
	if admin == nil {
		t.Fatal("admin user not in list")
	}
	if admin.Role != model.RoleAdmin {
		t.Fatalf("admin role: want admin, got %q", admin.Role)
	}
}

func TestCreateUserSuccess(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	r, b := doJSON(t, "POST", ts.URL+"/api/users", adminTok, map[string]any{
		"username": "newuser", "password": "strongpass1", "role": "viewer",
	})
	if r.StatusCode != 201 {
		t.Fatalf("create user: %d %s", r.StatusCode, b)
	}
	var created struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(b, &created); err != nil {
		t.Fatal(err)
	}
	if created.Username != "newuser" || created.Role != model.RoleViewer || created.ID == 0 {
		t.Fatalf("bad create response: %s", b)
	}

	// The new user should appear in the list.
	r2, b2 := doJSON(t, "GET", ts.URL+"/api/users", adminTok, nil)
	if r2.StatusCode != 200 {
		t.Fatalf("list after create: %d %s", r2.StatusCode, b2)
	}
	var users []*model.User
	json.Unmarshal(b2, &users)
	found := false
	for _, u := range users {
		if u.Username == "newuser" {
			found = true
		}
	}
	if !found {
		t.Fatal("new user not in list after create")
	}
}

func TestCreateUserDuplicateUsername409(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	r, _ := doJSON(t, "POST", ts.URL+"/api/users", adminTok, map[string]any{
		"username": "admin", "password": "strongpass1", "role": "viewer",
	})
	if r.StatusCode != 409 {
		t.Fatalf("dup username: want 409, got %d", r.StatusCode)
	}
}

func TestCreateUserBadRole400(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	r, _ := doJSON(t, "POST", ts.URL+"/api/users", adminTok, map[string]any{
		"username": "badrole", "password": "strongpass1", "role": "superuser",
	})
	if r.StatusCode != 400 {
		t.Fatalf("bad role: want 400, got %d", r.StatusCode)
	}
}

func TestCreateUserShortPassword400(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	r, _ := doJSON(t, "POST", ts.URL+"/api/users", adminTok, map[string]any{
		"username": "shortpw", "password": "short", "role": "viewer",
	})
	if r.StatusCode != 400 {
		t.Fatalf("short password: want 400, got %d", r.StatusCode)
	}
}

func TestDeleteUserSelfDelete409(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// adminTok's session belongs to the seeded admin user. Find its id.
	u, _ := e.server.Store.UserByUsername(ctxBg(), "admin")

	r, _ := doJSON(t, "DELETE", ts.URL+"/api/users/"+strconv.FormatInt(u.ID, 10), adminTok, nil)
	if r.StatusCode != 409 {
		t.Fatalf("self-delete: want 409, got %d", r.StatusCode)
	}
}

func TestDeleteUserLastAdmin409(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Strategy: start with 2 admins, delete one (leaving 1), then attempt
	// to delete the last via an admin-scope API token (which has no
	// session.UserID, so the self-delete guard is bypassed and the
	// last-admin guard is the one that fires).

	u1, _ := e.server.Store.UserByUsername(ctxBg(), "admin")

	// Create a second admin user + session.
	hash, _ := bcrypt.GenerateFromPassword([]byte("second-admin-pw"), bcrypt.DefaultCost)
	admin2, err := e.server.Store.CreateUser(ctxBg(), "admin2", string(hash), model.RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}

	// Create an admin-scope API token for the last-admin-delete attempt.
	tokResp, b := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{
		"name": "delete-test", "scope": "admin",
	})
	if tokResp.StatusCode != 201 {
		t.Fatalf("create admin token: %d %s", tokResp.StatusCode, b)
	}
	var adminToken struct {
		Token string `json:"token"`
	}
	json.Unmarshal(b, &adminToken)

	// Delete the second admin (admin1 does this), leaving only admin1.
	r0, _ := doJSON(t, "DELETE", ts.URL+"/api/users/"+strconv.FormatInt(admin2.ID, 10), adminTok, nil)
	if r0.StatusCode != 204 {
		t.Fatalf("delete admin2: want 204, got %d", r0.StatusCode)
	}

	// Now only admin1 remains. The API token (no session.UserID) tries to
	// delete admin1 — self-delete guard doesn't fire, last-admin does.
	r, _ := doJSON(t, "DELETE", ts.URL+"/api/users/"+strconv.FormatInt(u1.ID, 10), adminToken.Token, nil)
	if r.StatusCode != 409 {
		t.Fatalf("last-admin delete: want 409, got %d", r.StatusCode)
	}
}

func TestDeleteUserSuccess(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Create a viewer, then delete it.
	_, b := doJSON(t, "POST", ts.URL+"/api/users", adminTok, map[string]any{
		"username": "todelete", "password": "strongpass1", "role": "viewer",
	})
	var created struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(b, &created)
	if created.ID == 0 {
		t.Fatal("create failed")
	}

	r, _ := doJSON(t, "DELETE", ts.URL+"/api/users/"+strconv.FormatInt(created.ID, 10), adminTok, nil)
	if r.StatusCode != 204 {
		t.Fatalf("delete user: want 204, got %d", r.StatusCode)
	}

	// Confirm it's gone.
	u, _ := e.server.Store.UserByID(ctxBg(), created.ID)
	if u != nil {
		t.Fatal("user still exists after delete")
	}
}

func TestResetUserPassword(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Create a viewer with a known password.
	_, b := doJSON(t, "POST", ts.URL+"/api/users", adminTok, map[string]any{
		"username": "resetpw", "password": "originalpass1", "role": "viewer",
	})
	var created struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(b, &created)

	// Reset the password.
	r, b2 := doJSON(t, "PUT", ts.URL+"/api/users/"+strconv.FormatInt(created.ID, 10)+"/password", adminTok, map[string]any{
		"password": "newpassword1",
	})
	if r.StatusCode != 200 {
		t.Fatalf("reset password: %d %s", r.StatusCode, b2)
	}

	// Verify the new password works by logging in.
	r3, b3 := doJSON(t, "POST", ts.URL+"/api/auth/login", "", map[string]any{
		"username": "resetpw", "password": "newpassword1",
	})
	if r3.StatusCode != 200 {
		t.Fatalf("login with reset password: %d %s", r3.StatusCode, b3)
	}

	// Old password should fail.
	r4, _ := doJSON(t, "POST", ts.URL+"/api/auth/login", "", map[string]any{
		"username": "resetpw", "password": "originalpass1",
	})
	if r4.StatusCode != 401 {
		t.Fatalf("old password should fail: want 401, got %d", r4.StatusCode)
	}
}

// ---------- Read-scope token still works ----------

func TestReadScopeTokenGETokPOST403(t *testing.T) {
	e := newTestEnv(t)
	ts := e.serve(t)

	// Create a read-scope token.
	_, b := doJSON(t, "POST", ts.URL+"/api/tokens", adminTok, map[string]any{
		"name": "readonly", "scope": "read",
	})
	var tok struct {
		Token string `json:"token"`
	}
	json.Unmarshal(b, &tok)

	// GET /api/jobs → 200 (read-scope = viewer-equivalent).
	r1, _ := doJSON(t, "GET", ts.URL+"/api/jobs", tok.Token, nil)
	if r1.StatusCode != 200 {
		t.Fatalf("read token GET jobs: want 200, got %d", r1.StatusCode)
	}

	// POST /api/jobs → 403.
	r2, _ := doJSON(t, "POST", ts.URL+"/api/jobs", tok.Token, map[string]any{
		"series": "S", "episode": "01", "episode_dir": "S/Ep 01",
		"script_type": "vpy", "flow_id": 1,
	})
	if r2.StatusCode != 403 {
		t.Fatalf("read token POST jobs: want 403, got %d", r2.StatusCode)
	}
}
