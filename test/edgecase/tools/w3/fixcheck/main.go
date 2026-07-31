// Command fixcheck parses each YAML file named on the command line through the
// real dsl.Parse (KnownFields(true) + Job.Validate) and prints what the
// controller would see. Used to verify W3 campaign fixtures offline before
// spending an API call on them; W1 shipped two payloads that 400'd because a
// key path was wrong.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/eirueimi/unified-cd/internal/dsl"
)

func main() {
	rc := 0
	for _, path := range os.Args[1:] {
		f, err := os.Open(path)
		if err != nil {
			fmt.Printf("%s: OPEN FAILED: %v\n", path, err)
			rc = 1
			continue
		}
		job, err := dsl.Parse(f)
		f.Close()
		if err != nil {
			fmt.Printf("%s: PARSE FAILED: %v\n", path, err)
			rc = 1
			continue
		}
		fmt.Printf("%s: OK\n", path)
		fmt.Printf("  name=%q native=%v agentSelector=%v requiredCaps=%v\n",
			job.Metadata.Name, job.Spec.Native, job.Spec.AgentSelector, dsl.RequiredCaps(job.Spec))
		for i, s := range job.Spec.Steps {
			kind := "run"
			extra := ""
			switch {
			case s.Cache != nil:
				kind = "cache"
				extra = fmt.Sprintf(" path=%q key=%q restoreKeys=%v ttlDays=%d",
					s.Cache.Path, s.Cache.Key, s.Cache.RestoreKeys, s.Cache.TTLDays)
			case s.UploadArtifact != nil:
				kind = "uploadArtifact"
				extra = fmt.Sprintf(" name=%q path=%q", s.UploadArtifact.Name, s.UploadArtifact.Path)
			case s.DownloadArtifact != nil:
				kind = "downloadArtifact"
				extra = fmt.Sprintf(" name=%q", s.DownloadArtifact.Name)
			}
			fmt.Printf("  step[%d] name=%q kind=%s%s\n", i, s.Name, kind, extra)
		}
		for _, p := range job.Spec.Params.Inputs {
			fmt.Printf("  param name=%q type=%q required=%v default=%v\n", p.Name, p.Type, p.Required, p.Default)
		}
		fmt.Println(strings.Repeat("-", 60))
	}
	os.Exit(rc)
}
