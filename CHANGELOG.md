# Changelog

## [0.1.5](https://github.com/krezh/noctune/compare/0.1.4...0.1.5) (2026-08-25)


### Features

* **api:** add graceful search worker shutdown ([9ac00eb](https://github.com/krezh/noctune/commit/9ac00eb8fa0a7c3fbb43454176d418cbfb0de9ec))
* **bot:** add graceful watcher shutdown ([14de10d](https://github.com/krezh/noctune/commit/14de10dc1bf144d35d75169351a8095a1c99f6e2))
* **player:** add graceful manager shutdown ([2d5b1a0](https://github.com/krezh/noctune/commit/2d5b1a07a765b4746ce3c90520e6ea94af617ca6))


### Bug Fixes

* **api:** close unauthorized SSE streams ([95cd7c4](https://github.com/krezh/noctune/commit/95cd7c4af078a23fa2e6f8d6f81d77a1ffc332f8))
* **api:** enforce same-origin mutations ([96fd27d](https://github.com/krezh/noctune/commit/96fd27d15ad623f5fa8a05faf0c0269f84bc7c91))
* **app:** coordinate graceful shutdown ([66d8eea](https://github.com/krezh/noctune/commit/66d8eea0428f9450cf8fcd35ae99db8ea7a8774a))
* **audio:** drain buffered frames before completion ([5083b6c](https://github.com/krezh/noctune/commit/5083b6c44d962ca95c11b88fa78cfb5ed76082eb))
* **audio:** make playback stop idempotent ([f1e554e](https://github.com/krezh/noctune/commit/f1e554edba0bec6a91bad4809d3fc4f46bb9b930))
* **audio:** propagate encoder and source failures ([b1eb890](https://github.com/krezh/noctune/commit/b1eb890b8507db7ec5f9d040f66c43384c988f37))
* **audio:** signal completion after cleanup ([ea30205](https://github.com/krezh/noctune/commit/ea3020534711cfc79559b2a8da39011e5e1ea01f))
* **auth:** preserve sessions across restarts ([9df9a24](https://github.com/krezh/noctune/commit/9df9a2447451fb1856be483fd7fe1325afc230af))
* **auth:** revoke logged-out sessions ([ab8b6c0](https://github.com/krezh/noctune/commit/ab8b6c0336359f5d245bf5d7f170bc45fd294bec))
* **config:** reject partial Discord OAuth credentials ([692863f](https://github.com/krezh/noctune/commit/692863f593b3d2a76e2b82adb8aad616665ccea0))
* **player:** cancel invalidated track resolution ([e78c40c](https://github.com/krezh/noctune/commit/e78c40c0f5f42e77263d191fc4b5f49e0881f3e3))
* **player:** cancel invalidated track startup ([a1b9af4](https://github.com/krezh/noctune/commit/a1b9af437ae73aedb5ca252923094aba4030411e))
* **player:** reject joins during shutdown ([d144715](https://github.com/krezh/noctune/commit/d1447156d5463be985238f73b4954c7ddbfa0b3f))
* **player:** reject stale playback transitions ([4b49da8](https://github.com/krezh/noctune/commit/4b49da8ac5906eff05e844f66baec625e0585121))
* **player:** serialize voice connection transitions ([4d35b83](https://github.com/krezh/noctune/commit/4d35b8304777418fce8db3e63f7edc9ef5979396))
* **server:** bound HTTP connection lifetimes ([3de9c4a](https://github.com/krezh/noctune/commit/3de9c4ab90b7421ae8e4959b51d6b52775c0ae71))
* **ui:** show complete loop button hover border ([6c85265](https://github.com/krezh/noctune/commit/6c8526559b3b4edca39696f47ddeab7011564d08))


### Tests

* cover lifecycle and playback invariants ([9f676a4](https://github.com/krezh/noctune/commit/9f676a453663c01d835b78d06de7d9fd0b5c9ed2))

## [0.1.4](https://github.com/krezh/noctune/compare/0.1.3...0.1.4) (2026-08-23)


### Bug Fixes

* **bot:** run voice channel status update async to unblock presence updates ([2b0d54e](https://github.com/krezh/noctune/commit/2b0d54e4ef48fd2a88d108918f4b8e790e631563))
* **web:** double player max-width from 640px to 1280px ([dd23abb](https://github.com/krezh/noctune/commit/dd23abbfdfa9cbd57582671f428e0f8b349a86d9))

## [0.1.3](https://github.com/krezh/noctune/compare/0.1.2...0.1.3) (2026-08-23)


### Features

* **player:** stream playlist tracks into queue as they resolve ([ece7737](https://github.com/krezh/noctune/commit/ece773752a39dbe3b7cd89082bb9660f76c224ca))


### Bug Fixes

* **player:** gofmt alignment after adding loadingPlaylist field ([9fa6ee1](https://github.com/krezh/noctune/commit/9fa6ee19587c2c19dc7e1b4509e44f9d4a18d6dc))

## [0.1.2](https://github.com/krezh/noctune/compare/0.1.1...0.1.2) (2026-08-23)


### Features

* **bot:** show now-playing in bot status and voice channel status ([bb2f267](https://github.com/krezh/noctune/commit/bb2f267def905c6ebe22745e35ccc60cb94cdc1a))
* **bot:** use Discord embeds for rich bot replies ([c72827b](https://github.com/krezh/noctune/commit/c72827b4144105e4cde9ee6d48e463afe2e0812a))
* **container:** update image golang (1.26 ➔ 1.27) ([#5](https://github.com/krezh/noctune/issues/5)) ([fa508a3](https://github.com/krezh/noctune/commit/fa508a3d838d5627e1991cc15738f2c1f752316f))


### Bug Fixes

* **audio:** fix stop latency, add starvation tracking and buffer logging ([e8d266a](https://github.com/krezh/noctune/commit/e8d266a28cdf2e9e32a968ddca7454199fee126c))
* **bot:** fix voice join deadlock and context propagation ([ded4b33](https://github.com/krezh/noctune/commit/ded4b33a16d3d494576bb41a9934f54990aa704e))

## [0.1.1](https://github.com/krezh/noctune/compare/0.1.0...0.1.1) (2026-08-22)


### Features

* Initial commit ([94d2b21](https://github.com/krezh/noctune/commit/94d2b217987d238b37380f31b6c03ccb3c685a6e))
* **web:** add live YouTube search autocomplete to queue search box ([5172554](https://github.com/krezh/noctune/commit/51725548bc7ab553a0b3c9ee3d633fc8c68be047))
