package app

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// --- health -----------------------------------------------------------------

type adminData struct {
	Health  Health
	Users   []User
	Me      *User
	CanEdit bool // this session may change accounts and restore backups
	Recent  []AuditEntry
}

func (a *App) handleAdmin(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.ListUsers()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	recent, err := a.store.AuditTrail(12, "", 0)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.render(w, r, "admin.html", "Health & backup", adminData{
		Health:  a.Health(),
		Users:   users,
		Me:      CurrentUser(r),
		CanEdit: a.canAdminister(r),
		Recent:  recent,
	})
}

// canAdminister is true when nobody has claimed the install yet, or when the
// signed-in account is an administrator. The first case is what lets a
// single-password install keep working with no accounts at all.
func (a *App) canAdminister(r *http.Request) bool {
	if !a.auth.Accounts() {
		return true
	}
	u := CurrentUser(r)
	return u != nil && u.Admin
}

// actor is the name recorded in the audit trail for this request. Without a
// named account there is nothing better to say than how they got in, which is
// the honest answer and the reason accounts exist.
func (a *App) actor(r *http.Request) string {
	if u := CurrentUser(r); u != nil {
		return u.Username
	}
	if a.auth.password != "" {
		return "shared password"
	}
	return "anonymous"
}

// --- activity ---------------------------------------------------------------

type activityData struct {
	Entries []AuditEntry
	Limit   int
}

func (a *App) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 200
	}
	entries, err := a.store.AuditTrail(limit, r.URL.Query().Get("entity"), 0)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.render(w, r, "activity.html", "Activity", activityData{Entries: entries, Limit: limit})
}

// --- backup and restore -----------------------------------------------------

func (a *App) handleBackup(w http.ResponseWriter, r *http.Request) {
	if !a.canAdminister(r) {
		http.Error(w, "only an administrator can download a backup", http.StatusForbidden)
		return
	}
	name := fmt.Sprintf("backoffice-%s.zip", time.Now().Format("2006-01-02-1504"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if err := a.WriteBackup(w); err != nil {
		// The body has almost certainly started by now, so this can only be
		// logged -- the truncated zip is what tells the person it failed.
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "downloaded a backup", "system", 0, name)
}

func (a *App) handleRestore(w http.ResponseWriter, r *http.Request) {
	if !a.canAdminister(r) {
		redirect(w, r, "/admin", "", "only an administrator can restore a backup")
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != "replace" {
		redirect(w, r, "/admin", "", `type "replace" to confirm — restoring discards everything currently here`)
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File["backup"]) == 0 {
		redirect(w, r, "/admin", "", "choose a backup file first")
		return
	}
	fh := r.MultipartForm.File["backup"][0]
	f, err := fh.Open()
	if err != nil {
		redirect(w, r, "/admin", "", err.Error())
		return
	}
	defer f.Close()

	rep, err := a.Restore(f)
	if err != nil {
		redirect(w, r, "/admin", "", err.Error())
		return
	}
	// The session key lives outside the database, so whoever ran the restore is
	// still signed in; the accounts they are signed in as, however, came from
	// the backup.
	if err := a.auth.refreshUserCount(); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "restored a backup", "system", 0,
		fmt.Sprintf("%s from schema %d, %d rows across %d tables, %d photos",
			fh.Filename, rep.From, rep.Rows, rep.Tables, rep.Photos))
	redirect(w, r, "/admin", fmt.Sprintf("Restored %d rows across %d tables and %d photos",
		rep.Rows, rep.Tables, rep.Photos), "")
}

// handleSweep deletes photo files nothing points at any more.
func (a *App) handleSweep(w http.ResponseWriter, r *http.Request) {
	if !a.canAdminister(r) {
		redirect(w, r, "/admin", "", "only an administrator can do that")
		return
	}
	n := a.SweepOrphans()
	if n == 0 {
		redirect(w, r, "/admin", "Nothing to sweep", "")
		return
	}
	a.store.Record(a.actor(r), "swept orphaned photos", "system", 0, plural(n, "file"))
	redirect(w, r, "/admin", fmt.Sprintf("Deleted %s nothing was pointing at", plural(n, "file")), "")
}

// handleRebuildThumbs regenerates missing thumbnails.
func (a *App) handleRebuildThumbs(w http.ResponseWriter, r *http.Request) {
	n := a.photos.RebuildThumbs()
	redirect(w, r, "/admin", fmt.Sprintf("Rebuilt %s", plural(n, "thumbnail")), "")
}

func (a *App) handlePruneAudit(w http.ResponseWriter, r *http.Request) {
	if !a.canAdminister(r) {
		redirect(w, r, "/admin", "", "only an administrator can do that")
		return
	}
	days, _ := strconv.Atoi(r.FormValue("days"))
	if days <= 0 {
		days = 90
	}
	n, err := a.store.PruneAudit(days)
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	redirect(w, r, "/admin", fmt.Sprintf("Removed %s older than %s", plural(int(n), "entry"), plural(days, "day")), "")
}

// --- accounts ---------------------------------------------------------------

func (a *App) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	if !a.canAdminister(r) {
		redirect(w, r, "/admin", "", "only an administrator can add accounts")
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	first := !a.auth.Accounts()
	username := strings.TrimSpace(r.FormValue("username"))
	id, err := a.store.CreateUser(username, r.FormValue("password"), r.FormValue("admin") != "")
	if err != nil {
		redirect(w, r, "/admin", "", err.Error())
		return
	}
	// An install that had no password at all has no signing key yet, and the
	// account just created is going to need one.
	if err := a.auth.ensureKey(a.sessionKeyPath()); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	if err := a.auth.refreshUserCount(); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "added an account", "user", id, username)

	if first {
		// Creating the first account turns the door on. Whoever just did it was
		// already allowed in, so sign them in as that account rather than
		// bouncing them to a login page a moment after they set it up.
		if CurrentUser(r) == nil {
			a.auth.issue(w, r, id)
		}
		redirect(w, r, "/admin", fmt.Sprintf(
			"Created %s as an administrator — sign-in now asks for a username", username), "")
		return
	}
	redirect(w, r, "/admin", "Added "+username, "")
}

func (a *App) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	me := CurrentUser(r)
	// Anyone may change their own password; only an administrator may touch
	// someone else's account or hand out administrator rights.
	own := me != nil && me.ID == id
	if !own && !a.canAdminister(r) {
		redirect(w, r, "/admin", "", "you can only change your own password")
		return
	}

	if pw := r.FormValue("password"); pw != "" {
		if err := a.store.SetPassword(id, pw); err != nil {
			redirect(w, r, "/admin", "", err.Error())
			return
		}
		a.store.Record(a.actor(r), "changed a password", "user", id, "")
	}
	if role := r.FormValue("role"); role != "" && a.canAdminister(r) {
		if err := a.store.SetAdmin(id, role == "admin"); err != nil {
			redirect(w, r, "/admin", "", err.Error())
			return
		}
		a.store.Record(a.actor(r), "changed a role", "user", id, role)
	}
	redirect(w, r, "/admin", "Account updated", "")
}

func (a *App) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if !a.canAdminister(r) {
		redirect(w, r, "/admin", "", "only an administrator can remove accounts")
		return
	}
	u, err := a.store.GetUser(id)
	if err != nil {
		redirect(w, r, "/admin", "", "no such account")
		return
	}
	if err := a.store.DeleteUser(id); err != nil {
		redirect(w, r, "/admin", "", err.Error())
		return
	}
	if err := a.auth.refreshUserCount(); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "removed an account", "user", id, u.Username)
	// Deleting yourself ends your own session, which is less surprising than
	// staying signed in as an account that no longer exists.
	if me := CurrentUser(r); me != nil && me.ID == id {
		a.auth.clear(w)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	redirect(w, r, "/admin", "Removed "+u.Username, "")
}
