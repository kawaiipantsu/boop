# Native GUI

Not implemented. The native GUI is milestone 13 and must not begin before the
core, TUI and WebUI are mature (PROJECT.md §4.5).

Wails is preferred if reusing the WebUI proves advantageous; Fyne is the
alternative. Whichever is chosen, platform-specific GUI tooling must stay off
the standard core/TUI cross-build path so `make build-all` keeps working
without CGO.
