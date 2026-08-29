# Changelog

All notable changes to Boop are documented here.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Repository foundation: Go module, directory layout, Makefile, CI skeleton.
- `boop version` with link-time build metadata (version, commit, dirty state).
- Core contracts: provider interface and capabilities, normalized provider
  errors, command execution result types, permission policy model, tool
  registry, transport-neutral event bus, configuration schema.
