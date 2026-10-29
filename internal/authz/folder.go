package authz

import (
	"errors"
	"strings"
	"unicode"
)

// NormalizeFolder validates a folder key and strips leading and trailing "/".
// A folder is a literal job-name prefix; comparison is byte-exact.
func NormalizeFolder(key string) (string, error) {
	for _, r := range key {
		if unicode.IsSpace(r) {
			return "", errors.New("invalid folder: whitespace is not allowed")
		}
	}
	if strings.ContainsAny(key, "*?[") {
		return "", errors.New(`invalid folder: folders are literal job-name prefixes, "*", "?", "[" are not allowed`)
	}
	key = strings.Trim(key, "/")
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", errors.New(`invalid folder: segments must be non-empty and must not be "." or ".."`)
		}
	}
	return key, nil
}

// Covers reports whether folder is a proper ancestor folder of job.
func Covers(folder, job string) bool {
	return strings.HasPrefix(job, folder+"/")
}

// Ancestors returns the proper ancestor folders of job, outermost first.
func Ancestors(job string) []string {
	var out []string
	for i := 0; i < len(job); i++ {
		if job[i] == '/' && i > 0 {
			out = append(out, job[:i])
		}
	}
	return out
}
