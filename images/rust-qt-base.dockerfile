# rust-qt-base — Rust + Qt 6 (QML) build/test toolchain (capability: rust-qt).
# Arch Linux, every package pinned to one Arch Linux Archive snapshot, so a
# rebuild installs the same versions (qt6-base 6.12.0 on 2026/10/10). The
# template build's own `pacman -Syu` reads the same pinned mirrorlist.
# Headless GUI tests: QT_QPA_PLATFORM=offscreen on Mesa llvmpipe, xvfb-run
# as a fallback; dbus-run-session + gnome-keyring for Secret Service tests.
# No agent tooling: Honey layers its worker on top (FROM this image).
FROM archlinux:base-devel
ARG ARCH_SNAPSHOT=2026/10/10
# The archive can stall single downloads (pacman drops them after 10 s):
# each pacman step gets three attempts, keeping what already downloaded;
# the final `pacman -Q` fails the build if any package is still missing.
ARG PKGS="git python nodejs-lts-krypton namcap \
  rust cargo-deny sccache mold clang cmake ninja pkgconf corrosion \
  qt6-base qt6-declarative qt6-svg qt6-multimedia qt6-shadertools qt6-tools \
  sqlite libsecret openssl fontconfig \
  mesa xorg-server-xvfb dbus gnome-keyring \
  ttf-dejavu noto-fonts noto-fonts-emoji ttf-jetbrains-mono-nerd"
RUN printf 'Server = https://archive.archlinux.org/repos/%s/$repo/os/$arch\n' "$ARCH_SNAPSHOT" > /etc/pacman.d/mirrorlist \
 && for i in 1 2 3; do pacman -Syyuu --noconfirm && break; sleep 30; done \
 && for i in 1 2 3; do pacman -S --noconfirm --needed $PKGS && break; sleep 30; done \
 && pacman -Q $PKGS >/dev/null \
 && pacman -Scc --noconfirm \
 && pacman -Q qt6-base | grep -q ' 6\.12\.0-' \
 && pacman -Q rust | grep -q ' 1:1\.99\.0-'
# Rust 1.99.0 from Arch's own package (cargo, rustc, rustfmt, clippy), pinned
# by the snapshot. No rustup: leases may run offline (vendored crates,
# net.offline), and nothing may need a toolchain download at run time.
# (rustup's component download also panics inside the BuildKit sandbox.)
ENV CARGO_HOME=/root/.cargo
RUN rustc --version | grep -q '^rustc 1\.99\.0' \
 && cargo fmt --version && cargo clippy --version && cargo deny --version \
 && command -v qmake6 \
 && test -x /usr/lib/qt6/bin/qmllint && test -x /usr/lib/qt6/bin/qmltestrunner
COPY --chmod=755 guest/spoond-guest-init /usr/local/bin/spoond-guest-init
ARG SPOOND_GUEST_DNS_ADDR=
RUN mkdir -p /etc/spoond/init.d \
 && printf '%s\n' "$SPOOND_GUEST_DNS_ADDR" > /etc/spoond/guest-dns
