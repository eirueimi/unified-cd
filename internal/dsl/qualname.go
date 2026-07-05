package dsl

import "strings"

// QualifyName joins a directory path and a short name into a single qualified
// name (e.g. "team-a" + "build" -> "team-a/build"). Leading/trailing slashes on
// both parts are trimmed. An empty path yields the name unchanged.
func QualifyName(path, name string) string {
	path = strings.Trim(path, "/")
	name = strings.Trim(name, "/")
	if path == "" {
		return name
	}
	return path + "/" + name
}

// SplitQualifiedName splits a qualified name on its LAST slash into a directory
// path and a leaf. "team-a/build" -> ("team-a","build"); "build" -> ("","build").
func SplitQualifiedName(qualified string) (path, leaf string) {
	i := strings.LastIndex(qualified, "/")
	if i < 0 {
		return "", qualified
	}
	return qualified[:i], qualified[i+1:]
}

// QualifiedName returns the metadata's qualified name, folding in the reserved
// "path" annotation.
func (m Metadata) QualifiedName() string {
	return QualifyName(m.Annotations["path"], m.Name)
}
