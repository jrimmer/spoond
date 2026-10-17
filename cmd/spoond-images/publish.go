package spoondimages

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// publishOne republishes one image from the local registry to the
// publish prefix: pull <registry>/<name>:latest, tag it
// <prefix>/<name>:<tag>, push it, and remove both tagged copies again —
// the pull fills dockerd with an image only this command uses, so it is
// dropped exactly like buildOne's (spoond-07fn: the tagged local copies
// filled the root disk). The push must succeed: the rm only runs after
// it, so a failed push never deletes anything.
func publishOne(ctx context.Context, name, registry, prefix, tag string, stdout io.Writer) error {
	src := registry + "/" + name + ":latest"
	dst := prefix + "/" + name + ":" + tag
	if _, err := runCapture(ctx, stdout, "docker", "pull", src); err != nil {
		return fmt.Errorf("docker pull %s: %w: build %s first (spoond images build %s)", src, err, name, name)
	}
	if _, err := runCapture(ctx, stdout, "docker", "tag", src, dst); err != nil {
		return fmt.Errorf("docker tag %s %s: %w", src, dst, err)
	}
	stderr, err := runCapture(ctx, stdout, "docker", "push", dst)
	if err != nil {
		return classifyPushErr(dst, err, stderr)
	}
	// Best effort, like buildOne's rm: a failed rm never fails the
	// publish.
	if err := runCmd(ctx, stdout, "docker", "image", "rm", dst, src); err != nil {
		fmt.Fprintf(stdout, "warning: docker image rm: %v\n", err)
	}
	return nil
}

// classifyPushErr turns a failed docker push into its actionable cause:
// a remote registry rejects an unauthenticated push, so the message says
// to `docker login` first. Any other failure is returned as-is with
// docker's stderr.
func classifyPushErr(dst string, err error, stderr string) error {
	msg := strings.ToLower(stderr)
	for _, marker := range []string{"unauthorized", "authentication required", "denied", "401", "403"} {
		if strings.Contains(msg, marker) {
			return fmt.Errorf("docker push %s: %w (docker login %s first)", dst, err, registryHost(dst))
		}
	}
	return fmt.Errorf("docker push %s: %w (%s)", dst, err, strings.TrimSpace(stderr))
}

// registryHost returns the host[:port] of a /-separated image ref —
// everything before the first slash.
func registryHost(ref string) string {
	host, _, ok := strings.Cut(ref, "/")
	if !ok {
		return ref
	}
	return host
}

func cmdPublish(ctx context.Context, names []string, all bool, manifestPath, registry, prefix, tag string) int {
	if !all && len(names) == 0 {
		fmt.Fprintln(os.Stderr, "spoond images: publish needs a name or --all")
		return 2
	}
	if prefix == "" {
		fmt.Fprintln(os.Stderr, "spoond images: publish needs --prefix (the images go to <prefix>/<name>:<tag>)")
		return 2
	}
	if tag == "" {
		fmt.Fprintln(os.Stderr, "spoond images: publish needs a non-empty --tag")
		return 2
	}

	m, err := loadManifest(manifestPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "spoond images: %v\n", err)
		return 1
	}

	var targets []manifestImage
	if all {
		for _, img := range m.Images {
			if img.Baked {
				targets = append(targets, img)
			}
		}
	} else {
		for _, name := range names {
			found := false
			for _, img := range m.Images {
				if img.Name == name {
					targets = append(targets, img)
					found = true
					break
				}
			}
			if !found {
				fmt.Fprintf(os.Stderr, "spoond images: %s: not in %s\n", name, manifestPath)
				return 1
			}
		}
	}

	// A failed image is reported and the rest still publish, so one
	// missing image does not take the whole catalog down; the exit code
	// says the run was incomplete.
	failed := 0
	for _, img := range targets {
		if err := publishOne(ctx, img.Name, registry, prefix, tag, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "spoond images: %s: %v\n", img.Name, err)
			failed++
			continue
		}
		fmt.Fprintf(os.Stderr, "published %s as %s/%s:%s\n", img.Name, prefix, img.Name, tag)
	}
	if failed > 0 {
		return 1
	}
	return 0
}
