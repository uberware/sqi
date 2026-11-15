// SPDX-License-Identifier: AGPL-3.0-or-later

// Package diskfill fills a volume, so a chaos run can make a disk full without
// writing anything of the server's or a worker's own. It writes a ballast
// file that leaves a chosen amount of free space, and optionally empty files
// that use up the volume's inodes, and removes both again. Fill is supported
// only on Linux, the one OS a disk_full fault runs on.
package diskfill
