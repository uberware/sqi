# Changelog

All notable changes to sqi are documented here.
Format follows [Conventional Commits](https://www.conventionalcommits.org/) and
[Keep a Changelog](https://keepachangelog.com/) conventions.

> This file is generated from the Conventional Commits history by
> [git-cliff](https://git-cliff.org). Do not edit it by hand — to change an
> entry, change the commit message. Regenerate with `make changelog` (see
> `docs/development.md`). It is refreshed during release prep and again by the
> release workflow, which bundles it into the release archives.

## [0.3.0] — 2026-08-18


### Bug Fixes

- Duplicate websocket push on subscribe ([#87](https://github.com/uberware/sqi/issues/87)) ([19b0607](https://github.com/uberware/sqi/commit/19b060770ff25a67dc94b2172e4b570a3353e008))
- Enforce task state machine on task status writes ([#97](https://github.com/uberware/sqi/issues/97)) ([9c386d4](https://github.com/uberware/sqi/commit/9c386d42c6f28a6ab9250a53916063fc1242b4c9))
- **web:** Upgrade react-router to 8.3.0 for GHSA-qwww-vcr4-c8h2 ([#101](https://github.com/uberware/sqi/issues/101)) ([09b3ac6](https://github.com/uberware/sqi/commit/09b3ac636b22f0d15674ee557157a9c8bdfef0c6))
- Atomic job submission ([#113](https://github.com/uberware/sqi/issues/113)) ([b1aa609](https://github.com/uberware/sqi/commit/b1aa609273526d11c92cd516934fbb6b6cf7b3fc))
- **scheduler:** Match attr.worker.os.family macos against a darwin worker ([#116](https://github.com/uberware/sqi/issues/116)) ([cdf0815](https://github.com/uberware/sqi/commit/cdf0815ef964a250d426d170855e566ba64f4f0a))
- **scheduler:** Resolve attr.worker.cpu.arch against a worker-reporte… ([#117](https://github.com/uberware/sqi/issues/117)) ([874c85f](https://github.com/uberware/sqi/commit/874c85fa29e22afd89a3eb447e1c18f97bc5aea3))


### Build

- Replace deprecated goreleaser archives keys so goreleaser check passes ([c5d75de](https://github.com/uberware/sqi/commit/c5d75de68e8bc7df23c5fd0c11e8e8529731d6c4))


### Documentation

- Correct project status and documentation drift ([#98](https://github.com/uberware/sqi/issues/98)) ([9026e68](https://github.com/uberware/sqi/commit/9026e68cba99bbb3cad386440c642d3b22e463af))
- Correct inaccurate and stale documentation across the repo ([5ac8fc4](https://github.com/uberware/sqi/commit/5ac8fc4a7aefd236ce52c8e7c22cd2c14ed27a18))
- Mark phase 3 and the EXPR extension as released in v0.3.0 ([8d6c278](https://github.com/uberware/sqi/commit/8d6c2782cc6b35ad6e003db6a9e641fbf664d263))


### Features

- Authentication infrastructure ([#80](https://github.com/uberware/sqi/issues/80)) ([15e72b6](https://github.com/uberware/sqi/commit/15e72b66037dca9c38fe108eeb91fe499d1bf7ae))
- Local accounts, login sessions, auth shell ([#81](https://github.com/uberware/sqi/issues/81)) ([a55e203](https://github.com/uberware/sqi/commit/a55e203cb3852c4f849d918853cc2bfb45b258e1))
- Api keys ([#82](https://github.com/uberware/sqi/issues/82)) ([7dcb0e0](https://github.com/uberware/sqi/commit/7dcb0e0dc49c043c517e09c81b699084f529aa7e))
- Role-based access control ([#83](https://github.com/uberware/sqi/issues/83)) ([3190510](https://github.com/uberware/sqi/commit/3190510f14c6055d5759ed33dd2de7b5f5058918))
- Job owner binding ([#84](https://github.com/uberware/sqi/issues/84)) ([8d7cf58](https://github.com/uberware/sqi/commit/8d7cf58c5c2b603eb31e2d9b6b4afb1b7167b387))
- Auth admin and testing ([#85](https://github.com/uberware/sqi/issues/85)) ([e1003da](https://github.com/uberware/sqi/commit/e1003daa5cdaf98acd8c4db12b1400894e1b52f4))
- LDAP and AD integration ([#86](https://github.com/uberware/sqi/issues/86)) ([4339e31](https://github.com/uberware/sqi/commit/4339e31bd086d0fcd3451dc38917b00662b5dc19))
- OIDC compatible SSO and standardization with LDAP support ([#95](https://github.com/uberware/sqi/issues/95)) ([842e198](https://github.com/uberware/sqi/commit/842e198031515da35df286bb5c7e4f860394a977))
- Task isolation run as user ([#99](https://github.com/uberware/sqi/issues/99)) ([785034d](https://github.com/uberware/sqi/commit/785034d7ece38ef23398f88cc05796c91c1cc839))
- Improved openJD conformance ([#100](https://github.com/uberware/sqi/issues/100)) ([2cdef4f](https://github.com/uberware/sqi/commit/2cdef4f78618cfdabad999d7559180c4a31ba585))
- Support the OpenJD EXPR extension ([#114](https://github.com/uberware/sqi/issues/114)) ([0682598](https://github.com/uberware/sqi/commit/068259857ae4c3557f47ebade8b17da11b33f402))
- Ffmpeg presets ([#115](https://github.com/uberware/sqi/issues/115)) ([91bb943](https://github.com/uberware/sqi/commit/91bb943281a5cc04c78220fc91819330e2eec364))
- Mistika render presets ([#118](https://github.com/uberware/sqi/issues/118)) ([fe14c99](https://github.com/uberware/sqi/commit/fe14c99ead5acc63e93bb29b9e59f20b19ba7b99))

## [0.2.0] — 2026-07-13


### Documentation

- Usage pools are product limits ([5f308b7](https://github.com/uberware/sqi/commit/5f308b7a417b68ae0f0056dec4f92cab071fbea8))
- Integrate MkDocs site with GitHub Pages deployment on release ([#65](https://github.com/uberware/sqi/issues/65)) ([34d41ce](https://github.com/uberware/sqi/commit/34d41cec3aeec0fdfdcc3fdb8affb5ebce6fe185))
- Update to version 0.2.0 ([#69](https://github.com/uberware/sqi/issues/69)) ([8da5a79](https://github.com/uberware/sqi/commit/8da5a796e6abbc434fa8a1de220d8a4f9d2b1d70))


### Features

- Openjd extension infrastructure ([#47](https://github.com/uberware/sqi/issues/47)) ([9df4536](https://github.com/uberware/sqi/commit/9df4536c18a5faff7b5a7554c6d7181c83bccfad))
- Product definition system ([#48](https://github.com/uberware/sqi/issues/48)) ([3dd93cf](https://github.com/uberware/sqi/commit/3dd93cf91320b3650dfa73d15f67d325e8b80686))
- Compute location entity and affinity ([#49](https://github.com/uberware/sqi/issues/49)) ([d7c0435](https://github.com/uberware/sqi/commit/d7c043583b3700fde31045a6b6b993108622d0b3))
- Product management UI ([#50](https://github.com/uberware/sqi/issues/50)) ([91dc3ba](https://github.com/uberware/sqi/commit/91dc3ba6234235ef207b4fec7e44981e0fc8ef1c))
- Product driven submission form ([#51](https://github.com/uberware/sqi/issues/51)) ([1796efb](https://github.com/uberware/sqi/commit/1796efbba754e0b7ff2d0edbb3b178ccc97798d5))
- Additional path translation modes ([#54](https://github.com/uberware/sqi/issues/54)) ([a6af224](https://github.com/uberware/sqi/commit/a6af22452feb6b15e7d9045952db5eac34d71bca))
- S3 storage support ([#56](https://github.com/uberware/sqi/issues/56)) ([e2824fa](https://github.com/uberware/sqi/commit/e2824fa7669d1045591aa087c4ed3864af76c563))
- Preset library integration ([#59](https://github.com/uberware/sqi/issues/59)) ([a7ca63c](https://github.com/uberware/sqi/commit/a7ca63ce20032e697074259139e83a43103908ac))
- List filters ([#60](https://github.com/uberware/sqi/issues/60)) ([5cfb876](https://github.com/uberware/sqi/commit/5cfb876cb09311451fbd3eee62c69aa94947072e))
- Dcc submitter framework ([#64](https://github.com/uberware/sqi/issues/64)) ([f216d08](https://github.com/uberware/sqi/commit/f216d086567fae3f00022b11dedcd68cf7f0a4e9))
- Auto detect worker capabilities ([#70](https://github.com/uberware/sqi/issues/70)) ([d2bfb2c](https://github.com/uberware/sqi/commit/d2bfb2c2fa29732bc5b4725c9563ceba97f3ddd5))
- Unschedulable ux ([#71](https://github.com/uberware/sqi/issues/71)) ([641b752](https://github.com/uberware/sqi/commit/641b7522d9ef25f5fe2836da77a847b4ff3c0768))
- Sqi chunk bounds ([#74](https://github.com/uberware/sqi/issues/74)) ([45b35c9](https://github.com/uberware/sqi/commit/45b35c9d6f1ba2b8acbb00a9b5c9db56edd8f341))
- Test job presets ([#75](https://github.com/uberware/sqi/issues/75)) ([c575315](https://github.com/uberware/sqi/commit/c575315cfbaaee742bbb5f9ea27ddbe0cbe84fe4))
- Auto retry failure limits ([#76](https://github.com/uberware/sqi/issues/76)) ([2e12fed](https://github.com/uberware/sqi/commit/2e12fed672bf20ec31a3c124720ce75695b8ad7c))
- Cross job dependencies ([#77](https://github.com/uberware/sqi/issues/77)) ([3ad1af8](https://github.com/uberware/sqi/commit/3ad1af83c55e7d3a7700d809cf0a1f80fbd3fb39))


### Refactoring

- Preset namespace sqi ([#73](https://github.com/uberware/sqi/issues/73)) ([3dae608](https://github.com/uberware/sqi/commit/3dae60875c921aaeebe4da90eb7317589384dd3c))


### Testing

- **integration:** Stage_locally end-to-end with a real worker binary ([#55](https://github.com/uberware/sqi/issues/55)) ([5daa9b5](https://github.com/uberware/sqi/commit/5daa9b57c29b9c9be021c085340b64020acc34b0))

## [0.1.0] — 2026-06-25


### Bug Fixes

- **lint:** Remove cyclop skip-tests option unsupported in golangci-lint v2 ([65cd7b3](https://github.com/uberware/sqi/commit/65cd7b330ec5231668d643ad4a03d101ab9aa12a))
- **lint:** Resolve golangci-lint errors ([204c0de](https://github.com/uberware/sqi/commit/204c0decbb1f14a7682a4c04febf7dc3941c8c0c))
- **test:** Darwin requires unix for memsize ([dea9175](https://github.com/uberware/sqi/commit/dea9175e9bdf195d94620480fa80bdb91c6845b7))
- Missed a file in the last commit ([ef43c78](https://github.com/uberware/sqi/commit/ef43c78f9c318940090c4573563b027653e4fd14))
- Raw string instead of identifier in test ([02d4721](https://github.com/uberware/sqi/commit/02d4721d7e42532ec6d965071ee691cc27d40f6e))
- Multiple farm queries ([91bdc02](https://github.com/uberware/sqi/commit/91bdc026022d30e055d33ce32718ee30427b4733))
- WorkQueue streams enforce DeliverAll ([778e2a2](https://github.com/uberware/sqi/commit/778e2a250bcf85bfdb9bee3c8b6800403bee163f))
- Possible race condition in cancelJob ([4371509](https://github.com/uberware/sqi/commit/437150930cedd1f4abc00c8a5209764b549890a8))
- Non-atomic patchJob ([54d77bd](https://github.com/uberware/sqi/commit/54d77bda3b7bc7a9bb18823f79ad5e96b99f93d8))
- **scheduler:** Propagate lifecycle context to NATS handlers; guard step completion ([9349abc](https://github.com/uberware/sqi/commit/9349abc39a0d96f5e01bc9abfc2efe662d42a3d8))
- **ws:** Send subscribe ack before replaying buffered events ([d5242aa](https://github.com/uberware/sqi/commit/d5242aa9713fffae2f702e29c1d3d8cb691ef5a9))
- Race condition on channel close ([845635e](https://github.com/uberware/sqi/commit/845635e6da7e9de715d03ef7fe5ad454bda1b6a2))
- Workers without a farm affiliation are valid ([5a70858](https://github.com/uberware/sqi/commit/5a708584ac214625b2e4d10c723374c4fbb06afc))
- Updated docker build ([4c03692](https://github.com/uberware/sqi/commit/4c03692dd7d1919920e2bfe38511d5db12ba52ee))
- Updated wasi-threads package to pass CI ([9cc12a6](https://github.com/uberware/sqi/commit/9cc12a6abd345949f8ce78103433c5f11c2bcc55))
- Locked napi versions for CI ([69f1072](https://github.com/uberware/sqi/commit/69f1072933a1226919280d755be5674f1c12014a))
- MacOS instructions use correct reverse-DNS for uberware.net ([b320476](https://github.com/uberware/sqi/commit/b320476ffa3539c20c6c83b3c0ca049faabe4971))
- NATS listens on all interfaces by default ([6a9f7d4](https://github.com/uberware/sqi/commit/6a9f7d49c2d3ade5d00d6011d8a1cd1ed26b6521))
- Inconsistency in rate limiting ([77fbfc0](https://github.com/uberware/sqi/commit/77fbfc0c12049d9b6e13369750082269a7ad0237))
- Separate build cache scopes for GHCR publish action ([4bfd50e](https://github.com/uberware/sqi/commit/4bfd50e0ab73698b2c990ca01627a044d2b980b3))
- Python 3.9 mypy ci validation ([a464d07](https://github.com/uberware/sqi/commit/a464d073145eb65ab48724a63d13b41d012ae636))
- **ui:** Serve SPA shell for root-based deep links instead of 404 ([12ddf0a](https://github.com/uberware/sqi/commit/12ddf0a521b35eca120c9567f50eff30362ccb85))
- Dropped message issues ([569a8ac](https://github.com/uberware/sqi/commit/569a8ac5c0a944d7e08837e8007e9c79298eb3b5))
- Formatting failure ([42ebc8f](https://github.com/uberware/sqi/commit/42ebc8f1e833ce37f6b614999b4ebd37be9b0062))
- Propagate failure anc cancel state across dependency steps ([29a9db5](https://github.com/uberware/sqi/commit/29a9db529d09582e5fd5e81be1ee8b8eaecc3324))
- Remove uv.lock accidentally added that broke dependabot ([b9c02b2](https://github.com/uberware/sqi/commit/b9c02b21dc098233e056fa6423515bf37047c1b9))
- Ignore dependabot issue with python 3.9 ([4cc641e](https://github.com/uberware/sqi/commit/4cc641e5459156bee8c6db90afdd026845963e81))
- Worker name ([17113d5](https://github.com/uberware/sqi/commit/17113d5ef6c5ccab9ff7506e4896182222eeb398))
- Ci lint ([1903dde](https://github.com/uberware/sqi/commit/1903dde77df25674b0f4511969c8b2190d21d659))
- Worker failed to resolve relative path ([0591a10](https://github.com/uberware/sqi/commit/0591a10308b24e6ba2503ec10dd071e1c949241e))
- Job steps and tasks did not scroll ([0e3e48d](https://github.com/uberware/sqi/commit/0e3e48db1fed1d3bf481b654aed855608b3476b8))
- Job timing and task cleanup ([f4ac54f](https://github.com/uberware/sqi/commit/f4ac54f5db1e99e45fa88d95100be7dd9bc71eba))
- **openjd:** Missing vcpu min compliance ([#26](https://github.com/uberware/sqi/issues/26)) ([21f59d6](https://github.com/uberware/sqi/commit/21f59d604f7af7db6ca1137705397d2df0d3c9de))
- **bus:** Flush lease subscription to avoid request/reply race ([720df16](https://github.com/uberware/sqi/commit/720df166108372cc2b5f398076ecb6ef35c26335))


### Build

- Makefile, golangci-lint, formatting hooks ([2057822](https://github.com/uberware/sqi/commit/20578225c405598c4554dcecbdd0e32d074d3c70))
- **web:** Eslint, prettier, strict typescript, vitest, ci integration ([54892f4](https://github.com/uberware/sqi/commit/54892f493a8036c28c9ec3f03fec607847c9e0e7))
- **web:** Wire npm build into makefile and goreleaser ([3598dc0](https://github.com/uberware/sqi/commit/3598dc031f58316df53c535256691977499ac766))


### CI

- Github actions, dependabot, security policy ([d1c8adf](https://github.com/uberware/sqi/commit/d1c8adf190c11fe3fc43778a54de7018a31f0f3a))
- Race detector and coverage gate ([746a50a](https://github.com/uberware/sqi/commit/746a50a41955aa1da3bab4b1a79f61f3d865f447))
- **client/py:** Lint/type/test matrix, integration job, and release wheel packaging ([2a95025](https://github.com/uberware/sqi/commit/2a95025b8bcdee6ac15146daeeeba9ac435fd978))
- Fix shellcheck SC2155 in cross-platform-runtime smoke step ([b3559be](https://github.com/uberware/sqi/commit/b3559becf52b177f3830f18d519640ecd84b6633))
- Extract rcodesign with --wildcards so GNU tar finds it on linux ([#43](https://github.com/uberware/sqi/issues/43)) ([0cadeff](https://github.com/uberware/sqi/commit/0cadeff44f90816c5a0bdd81ff99c39939b6acf1))
- Prevent goreleaser dirty-state abort from changelog and rcodesign tarball ([#44](https://github.com/uberware/sqi/issues/44)) ([82df3a9](https://github.com/uberware/sqi/commit/82df3a9de737349ecc46ccd2997be33bdf1d7d20))
- Zip darwin binaries before notarizing (apple rejects bare mach-o) ([#45](https://github.com/uberware/sqi/issues/45)) ([be964c1](https://github.com/uberware/sqi/commit/be964c17952ee81d2af369f6b67c6cd4f032ade2))
- Raise rcodesign notarization wait limit to 30 minutes ([#46](https://github.com/uberware/sqi/issues/46)) ([ae370c5](https://github.com/uberware/sqi/commit/ae370c5165eb70358e20d3b0be5f761de3a6ccba))


### Documentation

- Initial documentation ([773609a](https://github.com/uberware/sqi/commit/773609a289cc8986ddeb0bcf24f7c6d1fa0d49e4))
- **worker:** Readme, deployment, configuration, capabilities, docker, godoc, dev guide ([84a8d9c](https://github.com/uberware/sqi/commit/84a8d9c0e5eaaf41611e2c10506571b92200630f))
- **web:** Readme, dev guide, build docs, jsdoc, accessibility baseline ([2c44678](https://github.com/uberware/sqi/commit/2c446787151f09b87891bfbc4f6c2d0328edd5d0))
- **client/py:** Readme, reference doc, examples, docstrings, and dev guide ([e34d0dc](https://github.com/uberware/sqi/commit/e34d0dc2a0ca7bcc80ce3cf17802e021b965c0ef))
- Add cross-platform validation guide ([ff5f69e](https://github.com/uberware/sqi/commit/ff5f69ef72a4e3bb599bac1e6ce23c9aae2aebac))


### Features

- **cli:** Cobra scaffold with serve/version/migrate/config subcommands ([ffa1ad1](https://github.com/uberware/sqi/commit/ffa1ad1a7cb7a091e2ed095bb80797a6ed9cea7b))
- **config:** Layered config loading with example file ([596920a](https://github.com/uberware/sqi/commit/596920a2e9dcc1fc1bec3eb8b29811e7bf189ed5))
- **log:** Slog-based structured logging and request middleware ([0f34f1a](https://github.com/uberware/sqi/commit/0f34f1abdf623f7569788fa806edfae9513e455f))
- **obs:** Prometheus metrics, health endpoints, pprof ([f69b2da](https://github.com/uberware/sqi/commit/f69b2da37a89028dd4800fc6dfb1d772e6483a43))
- **store:** Sqlite driver, schema, migrations ([51c83b6](https://github.com/uberware/sqi/commit/51c83b667b8515730abbed0e6ac4a1df3f50a6b0))
- **store:** Repository interface and sqlite implementation ([96d9bf6](https://github.com/uberware/sqi/commit/96d9bf6859111788783a01ab2a38cf07493c607f))
- **store:** Backup utility and in-memory fake ([b762a78](https://github.com/uberware/sqi/commit/b762a78fa58334078df25d0a16d26f2c1eb49166))
- **bus:** Embed nats jetstream with streams and subjects ([62f8783](https://github.com/uberware/sqi/commit/62f87838c5fd44cad6f2e937313bf898470bcce6))
- **bus:** Typed client, consumers, reconnect, graceful shutdown ([c9aff40](https://github.com/uberware/sqi/commit/c9aff4003f0484540d5d5b7b8d982d6f7dbe77c4))
- **openjd:** Parser, validator, parameter-space expansion ([e07f8ea](https://github.com/uberware/sqi/commit/e07f8eaf418f5bd7f5ba3d7aed1705748f118bc8))
- **openjd:** Persistence, task state machine, dependency resolution ([b4d610f](https://github.com/uberware/sqi/commit/b4d610fd44a96c7e2a8969098fecf0b2c1566397))
- **scheduler:** Assignment loop and worker registry with heartbeats ([97dce0f](https://github.com/uberware/sqi/commit/97dce0ff62366c6e6623e38aa188baefadd3005d))
- **scheduler:** Task selection, matching, policy, license gating ([9edbdbc](https://github.com/uberware/sqi/commit/9edbdbc21cb11b48b34bdead7559e9756c865aff))
- **scheduler:** Attempts, cancellation, instrumentation ([d3a50bb](https://github.com/uberware/sqi/commit/d3a50bb09b116dead9f9953b886d3a9014988f91))
- **worker-protocol:** Wire protocol and server-side handlers ([750ff46](https://github.com/uberware/sqi/commit/750ff46b3d008999f112adab15a890bb84529f06))
- **worker/cli:** Worker entry point with start/version/config subcommands ([37605fc](https://github.com/uberware/sqi/commit/37605fc991176454c61a4d01c658880e9541377e))
- **storage:** Named locations and resolved-mode path translation ([bc28836](https://github.com/uberware/sqi/commit/bc2883687b3e83373cea46503f8c0b6a085f7aea))
- **worker/config:** Typed config struct, worker id persistence, layered loading, and validation ([6e8e885](https://github.com/uberware/sqi/commit/6e8e885fbe926bd4e15e88993555bdf53f7af124))
- **worker/capabilities:** Auto-detection, manual tag overrides, and tests ([1b753c3](https://github.com/uberware/sqi/commit/1b753c3d801bca634a40215f8e3f7d88e7bd8f65))
- **worker/config:** Example yaml, dry-run flag, and config loader tests ([5d46a5c](https://github.com/uberware/sqi/commit/5d46a5c34df88cbb42c3da2a4365db5337f9c4fe))
- **worker/obs:** Slog, health endpoints, prometheus metrics, pprof ([30726b1](https://github.com/uberware/sqi/commit/30726b1fe0a80b709cfa258ebeda9cdc2c6ba6ba))
- **worker/nats:** Remote nats client with reconnect and tls ([2acd61e](https://github.com/uberware/sqi/commit/2acd61e967cc00a6dc5cced43b991b551d0e3b64))
- **worker/nats:** Remote nats client lifecycle with graceful drain ([3ba266d](https://github.com/uberware/sqi/commit/3ba266d32086d46f1ae9a92e0200094f7826dd7e))
- **licenses:** Pool crud, checkout records, admission check ([7faa7de](https://github.com/uberware/sqi/commit/7faa7de3ed67d1c24723fa250f1f4f385abfd0ed))
- **api:** Chi router with standard middleware ([938c603](https://github.com/uberware/sqi/commit/938c603b71474a062297db71866dec61dbceb994))
- **api:** Job endpoints (submit, list, detail, patch, cancel) ([e9c7c45](https://github.com/uberware/sqi/commit/e9c7c45d95b92daf09d5db1e5511883cbeb11dd0))
- **api:** Task endpoints (list, detail, logs, retry) ([2261b58](https://github.com/uberware/sqi/commit/2261b58a0697e7a1c2ed9a2ec57a76105c3102d5))
- **api:** Worker endpoints and admin actions ([5b4d5ce](https://github.com/uberware/sqi/commit/5b4d5ce39ead0cc72e9635c8f6b210fb1a41f540))
- **worker/registration:** Register, deregister, re-register on reconnect ([4eaceaa](https://github.com/uberware/sqi/commit/4eaceaa65e4299c01f59e6466ab261dfdf794ecb))
- **api:** Farm, queue, storage-location, license-pool crud ([a5a7f73](https://github.com/uberware/sqi/commit/a5a7f7388587b96933fa60a4cc22150a728400e3))
- **api:** RFC 7807 error format ([2612457](https://github.com/uberware/sqi/commit/261245764215e2730aee16385f0e644fea3b97f6))
- **api:** Openapi spec ([650676a](https://github.com/uberware/sqi/commit/650676a2ddc067e6628bb7fe54b217cb83c5ab0a))
- **api:** Versioning headers ([751902d](https://github.com/uberware/sqi/commit/751902db2c354dba795ebd54a28d272ffbe8935c))
- **ws:** Upgrade handler and message envelope ([509c1a8](https://github.com/uberware/sqi/commit/509c1a8a7217208733286c1ccefedec0c5b2e508))
- **ws:** Subscriptions, nats fanout, lifecycle ([29797db](https://github.com/uberware/sqi/commit/29797dbc8606a0c082ec9424b98358e51bffc517))
- **ui:** Embedded static asset hosting with placeholder ([06e6b4f](https://github.com/uberware/sqi/commit/06e6b4f23df0a26d14f995065742160f74464e74))
- **discovery:** Mdns broadcast for the server ([ef31dd9](https://github.com/uberware/sqi/commit/ef31dd946eb787b3326230c77035e34f25b80d26))
- **store:** Add Statuses IN-filter to ListTasksOptions ([7e560c6](https://github.com/uberware/sqi/commit/7e560c620a1c2ad14359b21d5afb3f2531295b33))
- **api:** Add per-IP token-bucket rate limiting (20 req/s, burst 40) ([fd386ec](https://github.com/uberware/sqi/commit/fd386ec3455ab6002e9e99a5a94f133c054d2fa6))
- **worker/heartbeat:** Periodic heartbeat with status payload and reconnect watchdog ([0d1afd5](https://github.com/uberware/sqi/commit/0d1afd5a9f9c54be4e097fdb37cbebc14c7e61a5))
- **worker/discovery:** Mdns server discovery with explicit address fallback ([fe6e3d3](https://github.com/uberware/sqi/commit/fe6e3d315505a457815338e2fb7a275e6edb6464))
- **worker/pull:** Work assignment pull loop with ack/nack semantics ([7e5c51d](https://github.com/uberware/sqi/commit/7e5c51d3bee1460b731f2a4df5d20eda51eabda6))
- **worker/session:** Session lifecycle with working directory and environment setup/teardown ([006a37d](https://github.com/uberware/sqi/commit/006a37d9565813ae4a490eaac1d8892d2773914a))
- **worker/executor:** Bare-metal process executor with env, cwd, capture, timeout, concurrency, and tests ([a55c2ca](https://github.com/uberware/sqi/commit/a55c2cae035d1f89b1c5196f251f6c63032a124b))
- **worker/paths:** Resolved-mode path substitution, openjd path mapping file, and tests ([ec5eb41](https://github.com/uberware/sqi/commit/ec5eb411c945d0df00623ccf246be682ecee5d79))
- **worker/logs:** Log streaming to nats with sequence numbers, chunking, flush-on-exit, and tests ([3d9bb92](https://github.com/uberware/sqi/commit/3d9bb92c5a3aebee46969f953217a16ccc3533bc))
- **worker/openjd:** Openjd progress, status, and fail line interception with tests ([3b9a659](https://github.com/uberware/sqi/commit/3b9a65916f31de41a8c7c9367f2306068b608c2e))
- **worker/status:** Task status publisher with progress, session id, and shutdown flush ([f16b543](https://github.com/uberware/sqi/commit/f16b5434724f2582b8e039c710a786456a3de555))
- **worker/cancel:** Task cancellation with sigterm/sigkill grace period, windows support, and tests ([4066bec](https://github.com/uberware/sqi/commit/4066bec6dfee05dae54dd97a6cd3fe3e80b21c1a))
- **worker/shutdown:** Graceful shutdown with in-flight task drain and force-kill fallback ([b025bc6](https://github.com/uberware/sqi/commit/b025bc6e968e56064189f75978c39f4d461de0ff))
- **web:** Vite react-ts bootstrap with dev proxy and embed integration ([a500ff9](https://github.com/uberware/sqi/commit/a500ff92c52372a63aa8854eb9d22f06cf3c6234))
- **web:** Design tokens, global styles, app shell, nav, error boundary ([8feee09](https://github.com/uberware/sqi/commit/8feee09046fd18f915dbda478eec46014f26c935))
- **web/api:** Typed fetch client, domain types, tanstack query, list and mutation hooks ([050d195](https://github.com/uberware/sqi/commit/050d1956eb05ac69daafbdb76b8d8becc0a398b7))
- **web/ws:** Websocket client with reconnect, typed dispatch, react context, status badge ([0e842f6](https://github.com/uberware/sqi/commit/0e842f6976dbe5ac8362dceacf85e760c108917f))
- **web/routing:** React router, notfound page, active nav, url-driven job filter state ([c1fcb9b](https://github.com/uberware/sqi/commit/c1fcb9b8dd257af88aaf826ae6474d8d568bbb80))
- **web/jobs:** Job list with status filter, search, sort, per-row cancel, bulk cancel ([8f936f2](https://github.com/uberware/sqi/commit/8f936f276fbfad0b1b13c13ebd1844ae3f624cb7))
- **web/jobs:** Job detail with metadata, step breakdown, task table, retry, log link ([1fc00ba](https://github.com/uberware/sqi/commit/1fc00bae61dad06adea1bc138699da758529f1a3))
- **web/logs:** Log viewer with pagination, live tail, ansi color, line numbers, copy ([1c8f14b](https://github.com/uberware/sqi/commit/1c8f14baa90847baf3c7ff6ff7e5221b5f68fba3))
- **web/realtime:** Websocket-driven job list and detail updates, refresh fallback ([7f6a506](https://github.com/uberware/sqi/commit/7f6a506b8deed86bf37b487deff89b2bce20f5af))
- **web/submit:** Openjd submission form with queue selector, codemirror, validation errors, redirect ([1d0a486](https://github.com/uberware/sqi/commit/1d0a486192822012612f20cb7e6e0346bf383cc9))
- **web/workers:** Worker list with status filter, enable/disable, live updates, tag display ([53a2ad6](https://github.com/uberware/sqi/commit/53a2ad647113c857e87a8cbb541c2f5395a4b1c8))
- **web/workers:** Worker detail with capabilities, assigned tasks, enable/disable ([629ab5e](https://github.com/uberware/sqi/commit/629ab5e21610ccee6afde1cf66ccb8d5d757f218))
- **web/dashboard:** Farm summary cards, recent failures, live websocket updates ([7bbeb02](https://github.com/uberware/sqi/commit/7bbeb02c15afdb3adea7915bbfd28e218691b8cc))
- **web/ui:** Datatable, pagination, toast, copy button, relative time components ([f90db9f](https://github.com/uberware/sqi/commit/f90db9f43f3f18d6c589610fc1ab4525ff2ca9a7))
- Add farm and queue management UI with default seed on first startup ([bee009f](https://github.com/uberware/sqi/commit/bee009f6ca61bca665e5c3531063de8456def41b))
- Empty worker farm id will take on work from any farm ([312ee19](https://github.com/uberware/sqi/commit/312ee194d3a08c5ca6562e0e81bfd2f9c015a137))
- **client/py:** Project scaffolding with pyproject, ruff, mypy, pytest, and make targets ([4f9479e](https://github.com/uberware/sqi/commit/4f9479ed4faad880fad15a3b28fb9f9c66216c3f))
- **client/py:** Transport core with typed errors, retries, pagination, and health probes ([3679a00](https://github.com/uberware/sqi/commit/3679a00aafb3fb19f41dbb77cf80a4ed361e577b))
- **client/py:** Typed dataclass models with tolerant parsing and wire fixtures ([4d4db51](https://github.com/uberware/sqi/commit/4d4db516789272c3f4310aef2a2476bcc35e83e8))
- **client/py:** Raw openjd job submission with yaml/json/dict/path inputs ([6847948](https://github.com/uberware/sqi/commit/684794850f4fae242ed552e10436f373427c9611))
- **client/py:** Job listing, detail, pause/resume, priority, and cancel ([b5ad9d5](https://github.com/uberware/sqi/commit/b5ad9d5a4d0136b9ba7453af8b093328194c88d4))
- **client/py:** Task queries, retry, and cursor-based log tailing ([e535654](https://github.com/uberware/sqi/commit/e535654578f54c4aadbb755ba62ccfaf05f184db))
- **client/py:** Worker listing, detail, and enable/disable ([4156414](https://github.com/uberware/sqi/commit/4156414b9ac85228a7e0d0226f902115e9e50463))
- **client/py:** Farm, queue, storage-location, and license-pool crud ([2d6b16a](https://github.com/uberware/sqi/commit/2d6b16ade95e3d9dcc3c8d2dc4e3834d8e1f8984))
- **client/py:** Optional websocket event stream and live log tailing ([3dc47e4](https://github.com/uberware/sqi/commit/3dc47e445fea22767d4f7172c706312da54445df))
- **client/py:** Wait_for_job, submit_and_wait, and curated public api ([1395963](https://github.com/uberware/sqi/commit/13959639073cd111f87870d016ad18314e89c2cf))
- Style and cleanup ([5bc01e6](https://github.com/uberware/sqi/commit/5bc01e642d65df35449a5102b2e8cadf990b37c0))
- Usage pool tracking (renamed from license pools) ([f343552](https://github.com/uberware/sqi/commit/f343552c530928c2e174cc25cf2ab3a9f910be13))
- Usage pool tracking (renamed from license pools) ([#20](https://github.com/uberware/sqi/issues/20)) ([10416a5](https://github.com/uberware/sqi/commit/10416a52ba95b70e06540dc42fea91b306d00a3d))
- Named storage locations and resolved path translation ([45852cf](https://github.com/uberware/sqi/commit/45852cf48b419a5c3ecc0290cea4385893873239))
- Dark mode, style improvements, ui fixes ([6f9892c](https://github.com/uberware/sqi/commit/6f9892c5251b6d474d42561983a139969c1c96f3))
- Necessary openJD spec compliance ([eb49e7d](https://github.com/uberware/sqi/commit/eb49e7d24fa2261e5c59bfd00782f4a7085beeec))
- Diagnostic logs observability ([7b044fb](https://github.com/uberware/sqi/commit/7b044fb2249249aa94e2149d175542d1f61dedd1))
- Cpu scheduling ([#22](https://github.com/uberware/sqi/issues/22)) ([37152d1](https://github.com/uberware/sqi/commit/37152d10aa554c412ee3f1de67a388f9b12d80db))
- **openjd:** Spec compliant host requirements ([#25](https://github.com/uberware/sqi/issues/25)) ([78154c2](https://github.com/uberware/sqi/commit/78154c26d20c80cd2a0ca2ce55fd647377e2f518))
- Worker cleanup ([235ee44](https://github.com/uberware/sqi/commit/235ee442bf127dc9978f1e8cc51ef6db3e1f4235))
- Job cleanup ([#28](https://github.com/uberware/sqi/issues/28)) ([194183b](https://github.com/uberware/sqi/commit/194183bcfaee9ec3f0f51ecdc4e08b68db267c8e))
- Task control ([#29](https://github.com/uberware/sqi/issues/29)) ([522cc80](https://github.com/uberware/sqi/commit/522cc8085d03751510a5a4570210ce3637cf07c4))
- Retry jobs ([#31](https://github.com/uberware/sqi/issues/31)) ([180bed3](https://github.com/uberware/sqi/commit/180bed3f2c6f9c0aea84c0417c03702dde438493))
- List filtering ([#33](https://github.com/uberware/sqi/issues/33)) ([2f52921](https://github.com/uberware/sqi/commit/2f52921b21f49604139368b1c6d46a13bade12cd))
- **py:** Access to server and worker version info ([#41](https://github.com/uberware/sqi/issues/41)) ([7509d31](https://github.com/uberware/sqi/commit/7509d319596b281ccad2f8a6e1f29b25d64a03d0))


### Refactoring

- **openjd:** Replace string-prefix error detection with sentinel type ([fb963f7](https://github.com/uberware/sqi/commit/fb963f7bb394c43a78419d4f9b27bbccbfcc2886))


### Testing

- Unit coverage for config, openjd, store, scheduler ([80fc597](https://github.com/uberware/sqi/commit/80fc5979475f88111e5260b5b5e868ec1903567d))
- Rest handler tests and fuzz targets ([027b336](https://github.com/uberware/sqi/commit/027b336b8c22d1393b9398960a039d2a78807e13))
- Websocket unit tests ([98ed8d3](https://github.com/uberware/sqi/commit/98ed8d37d48b6a4fcf6f892323b076dafc949e1a))
- Integration harness and load benchmark ([6826f99](https://github.com/uberware/sqi/commit/6826f9982b071799ffd0b31f9e5a353c06118769))
- More coverage ([c8f48f8](https://github.com/uberware/sqi/commit/c8f48f8d48dcb831f0c657582bb455700ec78130))
- **client/py:** Integration harness and end-to-end submit/query/manage tests against real binaries ([5a54c9b](https://github.com/uberware/sqi/commit/5a54c9b08bd780fd0d842e85847a5e26c0f90e54))
- Backfill unit coverage for bus, scheduler, worker, cmd, and web components ([8c548e0](https://github.com/uberware/sqi/commit/8c548e031a478f3815f7ae0b0991f4022c759ab4))
- **e2e:** Add playwright suite, smoke script, and cross-platform CI ([3cd6199](https://github.com/uberware/sqi/commit/3cd6199cae200615f137692c0c876768a1369a02))


### Style

- Ui updates ([#23](https://github.com/uberware/sqi/issues/23)) ([0c905dc](https://github.com/uberware/sqi/commit/0c905dc8ea6fe49db9a90f81b1b828cadc75b0c9))
- **web:** Icon buttons ([#30](https://github.com/uberware/sqi/issues/30)) ([9fc6555](https://github.com/uberware/sqi/commit/9fc65554b218ba137b9b96cb36eb915e836f2ca3))
- **web:** More icon buttons ([#32](https://github.com/uberware/sqi/issues/32)) ([f261ae4](https://github.com/uberware/sqi/commit/f261ae40d4701d4b02d6b9d0fe3ab470666dee99))


