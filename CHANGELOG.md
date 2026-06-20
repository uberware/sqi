# Changelog

All notable changes to sqi are documented here.
Format follows [Conventional Commits](https://www.conventionalcommits.org/) and
[Keep a Changelog](https://keepachangelog.com/) conventions.

<!-- This file is regenerated automatically by git-cliff on every tagged release.
     Manual edits will be overwritten. See cliff.toml for configuration. -->

## [Unreleased]

### Breaking Changes

- `DELETE /api/v1/jobs/{id}` no longer cancels a job — it now hard-deletes the
  job and all its associated data (steps, tasks, attempts, logs) immediately.
  Job cancellation has moved to `POST /api/v1/jobs/{id}/cancel`.

### Added

- Automatic retention sweep: terminal jobs older than `scheduler.job_retention`
  (default `168h`) are hard-deleted on a background tick. Failed jobs are
  excluded by default; set `scheduler.job_retention_include_failed: true` to
  include them. A zero or negative value disables the sweep.
