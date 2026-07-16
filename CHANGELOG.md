# Changelog

## [0.3.0](https://github.com/loewenthal-corp/shpiel/compare/v0.2.0...v0.3.0) (2026-07-16)


### Features

* **s3:** ambient AWS credentials — IRSA web identity, no static keys ([#35](https://github.com/loewenthal-corp/shpiel/issues/35)) ([5b7224a](https://github.com/loewenthal-corp/shpiel/commit/5b7224a362f119765e59c26ff22dfcc6d98606e4))
* **xet:** global chunk-deduplication query + dedup metrics ([#38](https://github.com/loewenthal-corp/shpiel/issues/38)) ([2b0cadb](https://github.com/loewenthal-corp/shpiel/commit/2b0cadbdc3121626e47dd0b3c55d81dd2d34ee9e))
* **xet:** xorb store on s3 — the bucket doubles as the CAS store ([#34](https://github.com/loewenthal-corp/shpiel/issues/34)) ([d906dba](https://github.com/loewenthal-corp/shpiel/commit/d906dba3a9d065a66026c240c88931b2766df934))


### Bug Fixes

* **deps:** update module github.com/alecthomas/kong to v1.16.0 ([#49](https://github.com/loewenthal-corp/shpiel/issues/49)) ([527b0d0](https://github.com/loewenthal-corp/shpiel/commit/527b0d03d4eafb71a37f8b26bc3a5016546c72be))
* **deps:** update module golang.org/x/sync to v0.22.0 ([#39](https://github.com/loewenthal-corp/shpiel/issues/39)) ([ac4ee18](https://github.com/loewenthal-corp/shpiel/commit/ac4ee182a6116fbe2e941297ee6d7eb7152dd411))
* **renovate:** stop pinDigests from mangling the chart's own image tag ([#36](https://github.com/loewenthal-corp/shpiel/issues/36)) ([1123acd](https://github.com/loewenthal-corp/shpiel/commit/1123acd69f8021cce6cfc87b0095089dab6b0e01))

## [0.2.0](https://github.com/loewenthal-corp/shpiel/compare/v0.1.1...v0.2.0) (2026-07-07)


### Features

* **backend:** s3-compatible bucket backend (type: s3) ([#33](https://github.com/loewenthal-corp/shpiel/issues/33)) ([e972240](https://github.com/loewenthal-corp/shpiel/commit/e9722406be4b2297d1da55cc137ab0bf4342a661))


### Bug Fixes

* **website:** make website CI pass under the pnpm 10→11 upgrade ([#26](https://github.com/loewenthal-corp/shpiel/issues/26)) ([5d727ee](https://github.com/loewenthal-corp/shpiel/commit/5d727ee748eb80e876ac399ba88159cc2446eb0d))

## [0.1.1](https://github.com/loewenthal-corp/shpiel/compare/v0.1.0...v0.1.1) (2026-07-03)


### Bug Fixes

* give HTTP clients private transports ([0d381ca](https://github.com/loewenthal-corp/shpiel/commit/0d381ca35aa29496a2ea8f29260edb8b0cc1b185))
* zot-compatible chunked blob commits, /api/validate-yaml, xet error mapping ([#21](https://github.com/loewenthal-corp/shpiel/issues/21)) ([8171ff1](https://github.com/loewenthal-corp/shpiel/commit/8171ff14e013ceac581c18a58664a1e069a99d4c))

## [0.1.0](https://github.com/loewenthal-corp/shpiel/compare/v0.0.1...v0.1.0) (2026-07-02)


### Features

* M0 read path — HF-compatible relay with pull-through caching ([eaa9d9f](https://github.com/loewenthal-corp/shpiel/commit/eaa9d9f4f6501d1a4355bf8449c8949d10a463d0))
* M1 write path + OCI backend — push_to_hub lands artifacts in Zot ([cf352a3](https://github.com/loewenthal-corp/shpiel/commit/cf352a3861e7485b477e2ff954487187724a9ddb))
* M2 ops-ready — replication, audit, admin API, Helm chart, Spegel docs ([5180544](https://github.com/loewenthal-corp/shpiel/commit/51805440cbcce95f5f502f76b4bad5313621caaf))
* Xet protocol server — unmodified hub 1.x uploads and chunk-level reads ([7d49856](https://github.com/loewenthal-corp/shpiel/commit/7d49856bf3755115f366e4c5391357ba18a4f80c))


### Bug Fixes

* stop yamlfmt fighting release-please over Chart.yaml ([d3869ea](https://github.com/loewenthal-corp/shpiel/commit/d3869eaac76632c80b427dc4f608e332e7a6b6fa))
