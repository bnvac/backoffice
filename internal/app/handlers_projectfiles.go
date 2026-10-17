package app

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"strings"
)

// handleUploadProjectFiles takes one or more files onto a project.
func (a *App) handleUploadProjectFiles(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	dest := fmt.Sprintf("/projects/%d#files", id)
	if _, err := a.store.GetProject(id); err != nil {
		http.NotFound(w, r)
		return
	}
	extendDeadline(w)
	r.Body = http.MaxBytesReader(w, r.Body, maxProjectFile+(1<<20))
	if err := parseForm(r); err != nil {
		redirect(w, r, dest, "", "that is bigger than "+a.files.Limit())
		return
	}
	if r.MultipartForm == nil || len(r.MultipartForm.File["files"]) == 0 {
		redirect(w, r, dest, "", "choose a file first")
		return
	}
	note := r.FormValue("note")
	var added, same []string
	for _, fh := range r.MultipartForm.File["files"] {
		src, err := fh.Open()
		if err != nil {
			redirect(w, r, dest, "", err.Error())
			return
		}
		blob, sum, size, err := a.files.Save(src, path.Ext(fh.Filename))
		src.Close()
		if errors.Is(err, errTooBig) {
			redirect(w, r, dest, "", fh.Filename+" is bigger than "+a.files.Limit())
			return
		}
		if err != nil {
			redirect(w, r, dest, "", fh.Filename+": "+err.Error())
			return
		}
		f, isNew, err := a.store.AddProjectFile(ProjectFile{ProjectID: id, Name: fh.Filename,
			Blob: blob, SHA256: sum, Size: size, Note: note, Author: a.actor(r)})
		if err != nil {
			a.fail(w, err, http.StatusInternalServerError)
			return
		}
		if !isNew {
			same = append(same, f.Name)
			continue
		}
		label := f.Name
		if f.Rev > 1 {
			label += fmt.Sprintf(" (rev %d)", f.Rev)
		}
		added = append(added, label)
		a.store.Record(a.actor(r), "added a file", "project", id, label)
	}
	msg := ""
	if len(added) > 0 {
		msg = "Added " + strings.Join(added, ", ")
	}
	warn := ""
	if len(same) > 0 {
		warn = strings.Join(same, ", ") + " had not changed since the last upload, so no new revision"
	}
	redirect(w, r, dest, msg, warn)
}

// handleProjectFile downloads one revision under the name it was uploaded as.
// Pictures and PDFs open in the browser; everything else downloads.
func (a *App) handleProjectFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	f, err := a.store.GetProjectFile(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p, ok := a.files.Path(f.Blob)
	if !ok {
		http.NotFound(w, r)
		return
	}
	ext := strings.ToLower(path.Ext(f.Name))
	inline := map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true, ".pdf": true}[ext]
	ctype := "application/octet-stream"
	disp := "attachment"
	if inline {
		ctype = mime.TypeByExtension(ext)
		disp = "inline"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, f.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// An uploaded file is never allowed to run as a page of this site.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; style-src 'unsafe-inline'; sandbox")
	http.ServeFile(w, r, p)
}

func (a *App) handleDeleteProjectFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	f, err := a.store.GetProjectFile(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := a.store.DeleteProjectFile(id); err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	a.store.Record(a.actor(r), "deleted a file", "project", f.ProjectID, fmt.Sprintf("%s rev %d", f.Name, f.Rev))
	redirect(w, r, fmt.Sprintf("/projects/%d#files", f.ProjectID), fmt.Sprintf("Deleted %s rev %d", f.Name, f.Rev), "")
}

// handlePrintProjectFile sends a project's G-code to a printer. It goes through
// the same path as a file sent from the Printing page — spool picked, grams
// taken, warnings given — and the job is tagged with the project, so its
// filament and time count towards what the build cost.
func (a *App) handlePrintProjectFile(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	extendDeadline(w)
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	f, err := a.store.GetProjectFile(id)
	if err != nil || !f.Printable() {
		http.NotFound(w, r)
		return
	}
	dest := fmt.Sprintf("/projects/%d#files", f.ProjectID)
	printerID := optionalID(r.FormValue("printer_id"))
	if printerID == nil {
		redirect(w, r, dest, "", "pick a printer")
		return
	}
	p, err := a.store.GetPrinter(*printerID)
	if err != nil {
		redirect(w, r, dest, "", "that printer is not here any more")
		return
	}

	// The printing side keeps its own copy, so the job history still works if
	// the file is later deleted from the project. The same bytes are stored once.
	src, err := a.files.Open(f.Blob)
	if err != nil {
		redirect(w, r, dest, "", "the stored file is missing — upload it again")
		return
	}
	blob, sum, size, err := a.gcode.Save(src, path.Ext(f.Name))
	src.Close()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	pid := f.ProjectID
	job := PrintJob{PrinterID: p.ID, ProjectID: &pid, Name: GcodeName(f.Name), Blob: blob, SHA256: sum, Size: size}
	r.Form.Set("next", dest)
	a.sendJob(w, r, p, job, r.FormValue("start") != "", r.FormValue("spool_id"))
}
