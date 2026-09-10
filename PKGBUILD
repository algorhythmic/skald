# Local package build: `makepkg -si` from this repository.
# Builds the working tree directly, so uncommitted changes are included and no
# tag or network fetch is needed. Go comes from .tools/go or PATH.
pkgname=skald
pkgver=0.1.0
pkgrel=1
pkgdesc="Conversation context across projects, terminals and environments"
arch=('x86_64' 'aarch64')
url="https://github.com/algorhythmic/skald"
license=('custom')
options=('!debug')
source=()

build() {
	cd "$startdir"
	export GOCACHE="$startdir/.cache/go-build"
	export GOMODCACHE="$startdir/.cache/go-mod"
	local gobin=go
	if [[ -x .tools/go/bin/go ]]; then
		gobin="$startdir/.tools/go/bin/go"
	fi
	"$gobin" build -buildvcs=false -trimpath -o "$srcdir/skald" ./cmd/skald
}

package() {
	install -Dm755 "$srcdir/skald" "$pkgdir/usr/bin/skald"
	install -Dm644 "$startdir/README.md" -t "$pkgdir/usr/share/doc/$pkgname/"
}
