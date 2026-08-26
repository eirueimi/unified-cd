package objectstore

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/minio/minio-go/v7/pkg/credentials"
)

// NewFileCredentials returns credentials read from path, re-read whenever the
// file changes.
//
// The file is the seam that makes refreshable credentials possible. A Secret
// consumed through envFrom is snapshotted into the container's environment when
// the container is created and never updates, so rewriting it leaves a running
// Pod holding the old value. A Secret mounted as a volume IS updated by the
// kubelet, which is why this provider watches the file rather than reading it
// once at startup.
//
// minio-go ships credentials.FileAWSCredentials, which reads the AWS shared
// INI format ($HOME/.aws/credentials, [profile] sections, aws_access_key_id /
// aws_secret_access_key keys). It was considered and rejected for this seam:
// the file this project mounts is a Kubernetes Secret projected as a single
// key holding UNIFIED_S3_KEY=.../UNIFIED_S3_SECRET=... lines (see
// internal/k8sagent/podbuilder.go), not an AWS profile file, so the INI
// parser would not read it. Writing a second, project-shaped provider is
// therefore not a second implementation of the same thing.
func NewFileCredentials(path string) *credentials.Credentials {
	return credentials.New(&fileProvider{path: path})
}

// fileProvider is a credentials.Provider that re-reads path on every
// IsExpired/Retrieve cycle whose last read no longer matches the file on
// disk. minio-go's Credentials.Get() calls IsExpired() first and only calls
// Retrieve() when it reports true (see the vendored
// pkg/credentials/credentials.go), so IsExpired is where the change
// detection has to live — Retrieve alone would only ever run once.
type fileProvider struct {
	path string

	// mu guards every field below: minio-go calls a Provider from whichever
	// goroutine is signing a request, and the sidecar can have an upload and
	// a download in flight at once, each capable of triggering a refresh.
	mu        sync.Mutex
	lastStat  fileStamp
	haveStamp bool
}

// fileStamp identifies the exact bytes last read, by their digest.
//
// An earlier version used mtime+size, which is the cheap answer and is wrong
// for this file in particular. A credential rotation replaces one key with
// another of the SAME length — an S3 access key ID is 20 characters and a
// secret 40, so size is not merely "usually" unchanged, it is essentially
// always unchanged. That leaves mtime alone carrying the signal, and mtime is
// coarse: NTFS and some Linux filesystems quantise it, so a rewrite landing
// inside one tick is invisible. The failure is silent and lands on the one
// path this seam exists for — a rotated credential never reaching the client,
// which is exactly the state the static key pair was replaced to avoid.
//
// Hashing costs one read of a file that is a few dozen bytes long, on a code
// path that was already doing an os.Stat syscall per call. That is the right
// trade for a detector that cannot miss.
type fileStamp struct {
	digest [sha256.Size]byte
	size   int64
}

// statStamp reads path and digests it. Named "stat" historically; it now
// reads, because stat metadata cannot answer the question (see fileStamp).
func statStamp(path string) (fileStamp, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return fileStamp{}, err
	}
	return fileStamp{digest: sha256.Sum256(b), size: int64(len(b))}, nil
}

// IsExpired reports whether the file's contents differ from the last
// successful read, or whether there has not been a successful read yet. A
// read failure (the file was briefly absent during a kubelet update, say) is
// treated as "not expired yet" so a transient error doesn't force every
// signing goroutine to hit Retrieve and surface the same transient error —
// Retrieve does its own open and will report the real error if the file is
// still missing when it actually runs.
func (p *fileProvider) IsExpired() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.haveStamp {
		return true
	}
	stamp, err := statStamp(p.path)
	if err != nil {
		return false
	}
	return stamp != p.lastStat
}

// Retrieve implements the deprecated single-value Provider method that
// RetrieveWithCredContext delegates to; kept because credentials.Provider
// still requires it.
func (p *fileProvider) Retrieve() (credentials.Value, error) {
	return p.retrieve()
}

// RetrieveWithCredContext ignores cc: the file read needs no HTTP client or
// endpoint, unlike the STS/IAM providers this interface method exists for.
func (p *fileProvider) RetrieveWithCredContext(_ *credentials.CredContext) (credentials.Value, error) {
	return p.retrieve()
}

func (p *fileProvider) retrieve() (credentials.Value, error) {
	f, err := os.Open(p.path)
	if err != nil {
		return credentials.Value{}, fmt.Errorf("read credential file %s: %w", p.path, err)
	}
	defer f.Close()

	var key, secret string
	var haveKey, haveSecret bool
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(name) {
		case "UNIFIED_S3_KEY":
			key = value
			haveKey = true
		case "UNIFIED_S3_SECRET":
			secret = value
			haveSecret = true
		}
	}
	if err := scanner.Err(); err != nil {
		return credentials.Value{}, fmt.Errorf("read credential file %s: %w", p.path, err)
	}

	var missing []string
	if !haveKey {
		missing = append(missing, "UNIFIED_S3_KEY")
	}
	if !haveSecret {
		missing = append(missing, "UNIFIED_S3_SECRET")
	}
	if len(missing) > 0 {
		return credentials.Value{}, fmt.Errorf("credential file %s: missing %s", p.path, strings.Join(missing, ", "))
	}

	stamp, err := statStamp(p.path)
	if err != nil {
		// The file existed for the Open above but vanished before this Stat —
		// vanishingly unlikely, but report it rather than caching a stamp
		// that can never again compare equal (a zero fileStamp legitimately
		// could).
		return credentials.Value{}, fmt.Errorf("stat credential file %s: %w", p.path, err)
	}

	p.mu.Lock()
	p.lastStat = stamp
	p.haveStamp = true
	p.mu.Unlock()

	return credentials.Value{
		AccessKeyID:     key,
		SecretAccessKey: secret,
		SignerType:      credentials.SignatureV4,
	}, nil
}
