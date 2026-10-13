package server

import (
	"filmeditor/internal/render"
	"filmeditor/internal/schema"
)

// renderTo renders edl with the job's recipe to out.
func renderTo(j *job, edl schema.EDL, out string) error {
	j.mu.Lock()
	rec := j.recipe
	j.mu.Unlock()
	return render.Render(edl, rec, out)
}
