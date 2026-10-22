# Images

spoond ships no sandbox images. A lease runs an image the operator
built on the host with `spoond images build`: any Dockerfile works once
it carries spoond's guest pieces, which spoond installs as
`/var/lib/spoond/guest/` (from `images/guest/` in this repo).

## A Dockerfile spoond can run

Copy `spoond-guest-init` into the build context, then add these lines
to the Dockerfile after everything else it installs:

```dockerfile
COPY --chmod=755 spoond-guest-init /usr/local/bin/spoond-guest-init
ARG SPOOND_GUEST_DNS_ADDR=
RUN mkdir -p /etc/spoond/init.d \
 && printf '%s\n' "$SPOOND_GUEST_DNS_ADDR" > /etc/spoond/guest-dns
```

- `spoond-guest-init` is the template's start command. It points the
  guest's resolver at `/etc/spoond/guest-dns`, runs every executable in
  `/etc/spoond/init.d/` in order, and marks the guest ready
  (`/run/spoond-guest-ready`). The template is snapshotted after that,
  so anything a hook does (a database initialised, a toolchain
  warmed) is in every sandbox's restored disk and memory. A failing
  hook fails the build.
- `SPOOND_GUEST_DNS_ADDR` is passed by `spoond images build` from the
  host's setting of the same name (the resolvers sandboxes use). Empty
  keeps the image's own `resolv.conf`.
- The image needs `bash`. Anything a job runs inside the sandbox (for
  example `git` for the runner's checkout) is the image's to provide.
- For interactive `ssh` sessions through the gateway, the image also
  needs `tmux` and `spoond-tmux.sh` as `/etc/profile.d/spoond-tmux.sh`,
  and its name in `GATEWAY_SSH_IMAGES`.

## The manifest

`spoond images build` reads a manifest (`--manifest`, default
`images/manifest.yaml` under the working directory) and a build context
(`--context`, default `images`):

```yaml
images:
  - name: my-image          # the lease API's `image`
    capability: python      # optional: shown in /api/capabilities
    description: "Python 3.12 for CI jobs"
    baked: true             # built by --all, checked by doctor
    dockerfile: my-image.dockerfile
    vcpu: 2                 # 1 or an even number
    memory_mb: 1024         # even, >= 128
    disk_mb: 4096           # free space guaranteed in the rootfs
    env:                    # defaults for every process in the sandbox
      PATH: /usr/local/bin:/usr/bin:/bin
```

The other fields:

- `min_memory_mb` is the smallest memory a lease may ask for (default
  512, or `memory_mb` when smaller).
- `warm_sizes` names `SIZE_PRESETS` sizes built as extra snapshots, so
  a lease at that size starts warm instead of cold-booting.
- `from` names a catalog image this one builds on (the `BASE` build
  arg). Its sizes and env are inherited unless set.

## Building

```bash
cp /var/lib/spoond/guest/spoond-guest-init ./
spoond images build my-image      # or --all for every baked entry
spoond images list
```

`--base-registry <prefix>` pulls `<prefix>/<name>` instead of running
`docker build`, for images already published to a registry of yours
(`spoond images publish` puts them there). `spoond install images`
writes the guest pieces and, with `--images <manifest>`, builds that
manifest's baked images.
