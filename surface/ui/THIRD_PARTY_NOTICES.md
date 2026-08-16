# Third-party notices

## OpenCodex

- Source: <https://github.com/RyensX/OpenCodex>
- Reviewed revision: `f37ef2de77d8`
- License: GNU Affero General Public License, version 3 only
- License text: `LICENSE`

The entry/auth panel and settings-drawer CSS primitives in `src/styles.css`
are adapted from the readable source file `web-shell/index.html`. The code was
split into the pi Workbench stylesheet, renamed, and connected to pi-owned
application clients.

No compiled official Codex renderer, extracted application bundle, minified
renderer, encrypted code, or other generated OpenCodex artifact is included.
The remainder of the Workbench is original pi-go source unless a file says
otherwise.

## Runtime UI dependencies

The Workbench also uses React (MIT) and Lucide (ISC). Their exact versions are
locked in `package-lock.json`; their license files are installed with the
packages and must be retained when redistributing a bundled GUI.
