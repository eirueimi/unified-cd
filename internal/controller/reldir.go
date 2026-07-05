package controller

import (
	"path"
	"strings"
)

// relDir returns the directory of filePath relative to specPath (the AppSource
// root). "jobs/", "jobs/team-a/build.yaml" -> "team-a". A file directly under
// specPath yields "".
func relDir(specPath, filePath string) string {
	prefix := strings.Trim(specPath, "/")
	rel := strings.TrimPrefix(strings.TrimPrefix(filePath, prefix), "/")
	dir := path.Dir(rel)
	if dir == "." {
		return ""
	}
	return dir
}
