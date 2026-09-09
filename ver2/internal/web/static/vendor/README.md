# 벤더링된 서드파티 파일

NAS 가 인터넷에 닿지 않을 수 있으므로 CDN 을 쓰지 않고 바이너리에 포함합니다.

| 파일 | 버전 | 출처 | sha256 |
|---|---|---|---|
| `hls-1.5.20.min.js` | 1.5.20 | https://cdn.jsdelivr.net/npm/hls.js@1.5.20/dist/hls.min.js | `d016c1230496ee59f3f5b01c16cce4cc01b5a1d3d357adec200c908b131ebe49` |

라이선스는 `hls.js-LICENSE` (Apache-2.0).

Safari 와 iOS 는 HLS 를 네이티브로 재생하므로 이 파일을 내려받지 않습니다.
파일명에 버전이 들어 있어 캐시를 영구히 잡아도 안전합니다.
