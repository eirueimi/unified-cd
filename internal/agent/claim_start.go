package agent

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eirueimi/unified-cd/internal/imageref"
	crt "github.com/eirueimi/unified-cd/internal/runtime"
)

const (
	// defaultPodPullTimeout bounds a claim pod's image-pull step.
	defaultPodPullTimeout = 30 * time.Minute
	// defaultPodStartTimeout bounds creating and starting the pod's
	// containers once the images are local.
	defaultPodStartTimeout = 5 * time.Minute
)

// Sentinels used as the cause of the pull and start deadlines, so the
// caller can tell its own timeout from a parent cancellation.
var (
	errPullTimeout  = errors.New("claim pod image pull timed out")
	errStartTimeout = errors.New("claim pod start timed out")
)

func pullTimeoutMsg(d time.Duration) string {
	return fmt.Sprintf("claim pod image pull exceeded podPullTimeout (%s)", d)
}

func startTimeoutMsg(d time.Duration) string {
	return fmt.Sprintf("claim pod start exceeded podStartTimeout (%s)", d)
}

func (a *Agent) podPullTimeout() time.Duration {
	if a.PodPullTimeout <= 0 {
		return defaultPodPullTimeout
	}
	return a.PodPullTimeout
}

func (a *Agent) podStartTimeout() time.Duration {
	if a.PodStartTimeout <= 0 {
		return defaultPodStartTimeout
	}
	return a.PodStartTimeout
}

// claimImages lists the claim pod's images: the pause image first, then the
// container images in order, each once.
func claimImages(defs []containerDef, pauseImage string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(img string) {
		if img == "" || seen[img] {
			return
		}
		seen[img] = true
		out = append(out, img)
	}
	add(pauseImage)
	for _, d := range defs {
		add(d.Image)
	}
	return out
}

// pullClaimImages makes every image available locally before any container is
// created. An image is pulled when it is mutable (imageref.AlwaysPull) or not
// present; a failed pull fails the claim, with no fallback to a stale local
// copy. With a PodRuntime it returns the post-pull image IDs; without one it
// pulls only the always-pull refs (presence cannot be checked) and returns a
// nil map.
func pullClaimImages(ctx context.Context, rt crt.ContainerRuntime, images []string) (map[string]string, error) {
	pr, hasPod := rt.(crt.PodRuntime)
	var ids map[string]string
	if hasPod {
		ids = make(map[string]string, len(images))
	}
	done := map[string]bool{}
	for _, img := range images {
		if done[img] {
			continue
		}
		done[img] = true
		pull := imageref.AlwaysPull(img)
		if hasPod && !pull {
			_, present, err := pr.ImageID(ctx, img)
			if err != nil {
				return nil, fmt.Errorf("inspect image %q: %w", img, err)
			}
			pull = !present
		}
		if pull {
			if err := rt.Pull(ctx, img); err != nil {
				return nil, fmt.Errorf("pull image %q: %w", img, err)
			}
		}
		if hasPod {
			id, present, err := pr.ImageID(ctx, img)
			if err != nil {
				return nil, fmt.Errorf("inspect image %q: %w", img, err)
			}
			if !present {
				return nil, fmt.Errorf("image %q is not present after pull", img)
			}
			ids[img] = id
		}
	}
	return ids, nil
}
