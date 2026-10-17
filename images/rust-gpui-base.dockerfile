# rust-gpui-base — rust-qt-base plus the native libraries gpui (Zed's GUI
# crate, 0.2.2 on crates.io) needs on Linux (capability: rust-gpui).
# A layer: the manifest entry has `from: rust-qt-base`, so spoond images
# build passes the base's current digest as BASE. The base's pinned Arch
# Linux Archive mirrorlist is still in place, so pacman here installs from
# the same snapshot as the base and rebuilding the base moves both.
#
# What gpui 0.2.2 needs, from its Cargo.toml and a built binary's ldd:
#   link time   libxcb, libxkbcommon, libxkbcommon-x11 (xkbcommon crate
#               with the x11 feature; zed-scap's xcb crate)
#   build time  pkg-config files for wayland, freetype2 and fontconfig
#               (wayland-sys, freetype-sys, yeslogic-fontconfig-sys build
#               scripts); wayland-protocols and vulkan-headers for any
#               crate that generates from the system XML or headers
#   run time    dlopen'd: libvulkan (blade renders only through Vulkan),
#               libwayland-client; vulkan-swrast is Mesa's lavapipe, a
#               software Vulkan driver, so a window can be drawn in a
#               sandbox with no GPU (xvfb-run comes from the base)
# Not needed: alsa-lib (no alsa-sys in gpui's dependency tree); bindgen
# and cbindgen run only for macOS targets. git, python (python3, needed by
# the guest init hooks), clang, pkgconf, mold and sccache come from the base.
ARG BASE
FROM ${BASE}
# resolveBase always passes SPOOND_GUEST_DNS_ADDR and, for a from: layer,
# WARM_ENV. The base already wrote /etc/spoond/guest-dns and this layer
# warms nothing; declaring them keeps docker from warning about unused
# build args.
ARG SPOOND_GUEST_DNS_ADDR=
ARG WARM_ENV=
ARG PKGS="libxcb libxkbcommon libxkbcommon-x11 \
  wayland wayland-protocols \
  vulkan-icd-loader vulkan-headers vulkan-swrast \
  freetype2 fontconfig git python"
# Same retry shape as the base: the archive can stall single downloads.
RUN for i in 1 2 3; do pacman -Syyuu --noconfirm && break; sleep 30; done \
 && for i in 1 2 3; do pacman -S --noconfirm --needed $PKGS && break; sleep 30; done \
 && pacman -Q $PKGS >/dev/null \
 && pacman -Scc --noconfirm
RUN pkg-config --exists xcb xkbcommon xkbcommon-x11 wayland-client \
      wayland-protocols vulkan freetype2 fontconfig \
 && test -e /usr/share/vulkan/icd.d/lvp_icd.json \
 && test -e /usr/lib/libvulkan.so.1 \
 && python3 --version && git --version \
 && rustc --version | grep -q '^rustc 1\.99\.0'
