# nvt — NAS 비디오 트랜스코딩 프록시

NAS 미디어 디렉터리를 **WebDAV**로 서빙합니다. 플레이어가 이미 재생할 수 있는
파일은 손대지 않고 그대로 내보내고, 재생하지 못하는 파일만 필요할 때 변환해서
`.mkv` 이름으로 제공합니다. 탐색 화면은 실제 공유 폴더를 그대로 보는 것과
똑같습니다.

Apple TV + Infuse, Synology/XPEnology 조합을 염두에 두고 만들었지만 어느 쪽에도
종속된 코드는 없습니다.

## 왜 WebDAV인가

Infuse는 SMB, FTP, SFTP, NFS, WebDAV, DLNA를 지원합니다. 이 중 **평범한 HTTP인
것은 WebDAV뿐**입니다. 덕분에 변환된 파일을 일반 파일과 완전히 같은 경로로
서빙할 수 있고, 바이트 범위(Range) 시크가 공짜로 따라옵니다. 앱 안에서의 탐색
경험은 지금 쓰시는 FTP와 같습니다.

## 무엇을 변환하고, 얼마나 걸리나

모든 파일은 `ffprobe`로 한 번 검사하고(결과는 디스크에 캐시), 세 가지 플랜 중
하나로 분류됩니다.

| 플랜 | ffmpeg | 속도 |
|---|---|---|
| `passthrough` | 없음 | 즉시 |
| `audio` | `-c:v copy -c:a flac` | 오디오 인코딩 속도에 좌우 (아래 참고) |
| `video` | `-c:v libx264 …` | 저전력 CPU에서는 실시간에 한참 못 미침 |

**중요한 것은 `audio` 플랜입니다.** 영상은 비트 단위로 그대로 복사되므로 화질
열화가 없습니다. 남는 비용은 오디오 인코딩 하나뿐입니다.

`video` 플랜은 완결성을 위해 넣어 두었습니다. Celeron J1900에서는 재생 속도를
따라가지 못하므로, 즉시 재생용이 아니라 야간 배치 작업으로 생각하셔야 합니다.

### 왜 FLAC 인가

`audio` 플랜의 비용은 사실상 전부 오디오 인코딩입니다. 그런데 손실 코덱(AAC,
MP3)은 심리음향 모델을 돌리기 때문에 저전력 CPU 에서 놀랄 만큼 느립니다.

Celeron J1900 에서 스테레오 오디오를 실측한 값입니다:

| 코덱 | 속도 | 52분 에피소드 |
|---|---|---|
| AAC 384k | 4x | 13분 |
| MP3 256k | 6x | 9분 |
| **FLAC** | **60x** | **1분 미만** |

같은 CPU 에서 AC3 디코딩은 71x, 영상 복사는 215x, 디스크 읽기는 191MB/s 였습니다.
병목은 오직 인코더였습니다.

FLAC 은 무손실이라 음질 손실도 없습니다. 대가는 용량입니다 — 스테레오 48kHz 기준
대략 700~900kb/s 로, 52분 에피소드에 300MB 쯤 붙습니다. 용량이 급하면
`NVT_AUDIO_CODEC=aac` 로 되돌리되 위의 시간을 감수해야 합니다.

### "재생할 수 없다"의 기준

어떤 코덱을 플레이어가 처리할 수 있는지는 코드에 박힌 규칙이 아니라 **설정
값**입니다.

```
NVT_VIDEO_OK=h264,hevc,mpeg4,msmpeg4v3,mpeg2video,...
NVT_AUDIO_OK=aac,mp3,mp2,flac,alac,opus,vorbis,pcm_s16le,...
```

기본값은 무료 버전 Infuse를 가정합니다 — 영상 코덱은 대체로 문제없고, 라이선스가
걸린 서라운드 오디오(AC3, E-AC3, DTS, DTS-HD, TrueHD)가 막힌다는 전제입니다.
**이 전제를 본인 라이브러리로 직접 확인하세요.** Firecore의 공개 문서는 무료와
Pro의 코덱 경계를 명시하지 않으며, 증상을 보면 어느 쪽인지 알 수 있습니다.

- 영상은 나오는데 소리가 없다 → 오디오 코덱
- 아예 재생이 안 된다 → 영상 코덱 또는 컨테이너

영상 쪽이 원인으로 밝혀지면 목록에서 해당 코덱을 빼면 됩니다. 어느 쪽이든
파이프라인은 동일합니다.

## 배포

`/dev/dri` 패스스루는 필요 없습니다. 일반적인 경로에서는 영상을 인코딩하지
않으므로 하드웨어 가속이 무의미합니다.

### 방법 1: 미리 빌드된 이미지 받기 (권장)

`main`에 푸시하면 GitHub Actions가 `linux/amd64` 이미지를 빌드해
`ghcr.io/kang3q/nas-video-transcoding:latest`로 발행합니다. NAS는 내려받기만
하므로 **아무것도 컴파일하지 않습니다.**

NAS에 소스는 필요 없고 compose 파일 하나면 됩니다.

```bash
mkdir -p /volume1/docker/nvt/cache && cd /volume1/docker/nvt
curl -sO https://raw.githubusercontent.com/kang3q/nas-video-transcoding/main/docker-compose.nas.yml
mv docker-compose.nas.yml docker-compose.yml
vi docker-compose.yml          # 볼륨 경로 두 줄 수정
sudo docker compose up -d
```

> Synology 의 Docker 는 바인드 마운트 경로를 자동으로 만들지 않습니다. 캐시
> 폴더가 없으면 `Bind mount failed: ... does not exist` 로 컨테이너가 뜨지
> 않으니, compose 의 경로를 바꿨다면 그 경로로 `mkdir -p` 를 먼저 하세요.

업데이트:

```bash
sudo docker compose pull && sudo docker compose up -d
```

`:latest` 이미지가 로컬에 남아 있으면 Docker 는 다시 받지 않습니다. Container
Manager 에서 프로젝트를 지워도 이미지는 남으므로, 새 버전이 안 잡히면 위의
`pull` 을 쓰거나 이미지를 직접 지우세요:

```bash
sudo docker compose down
sudo docker rmi ghcr.io/kang3q/nas-video-transcoding:latest
sudo docker compose up -d
```

새 이미지가 떴는지는 기동 로그의 `prefetch=... max=... video=...` 줄로
확인할 수 있습니다. 특정 버전에 고정하려면 `:latest` 대신 커밋 태그를
쓰세요 (예: `sha-bafa722`).

> 첫 발행 직후 ghcr.io 패키지는 비공개입니다. GitHub 저장소 → Packages →
> 해당 패키지 → Package settings → Change visibility → Public으로 바꾸면
> NAS에서 로그인 없이 받을 수 있습니다.

### 방법 2: NAS에서 직접 빌드

소스를 NAS로 옮긴 뒤:

```bash
sudo docker compose up -d --build
```

J1900에서 첫 빌드는 golang 이미지를 받아야 해서 3~10분 걸립니다. 소스를 옮기지
않고 원격 저장소를 빌드 컨텍스트로 바로 쓸 수도 있습니다:

```bash
sudo docker build -t nvt https://github.com/kang3q/nas-video-transcoding.git
```

### Infuse 연결

**파일 추가 → WebDAV**, 주소는 `http://<나스IP>:8080/`.

외부에서 접속할 예정이라면 `NVT_USER`/`NVT_PASS`를 반드시 설정하세요.
기본값은 인증 없이 열려 있습니다.

## 설정

| 변수 | 기본값 | 의미 |
|---|---|---|
| `NVT_MEDIA_DIR` | `/media` | 원본 트리, 읽기 전용으로 마운트 |
| `NVT_CACHE_DIR` | `/cache` | 변환 결과물 |
| `NVT_LISTEN` | `:8080` | 리슨 주소 |
| `NVT_USER` / `NVT_PASS` | — | 선택적 basic auth. 비워 두면 인증 없음 |
| `NVT_VIDEO_OK` | 위 참조 | 변환이 필요 없는 영상 코덱 |
| `NVT_AUDIO_OK` | 위 참조 | 변환이 필요 없는 오디오 코덱 |
| `NVT_AUDIO_CODEC` | `flac` | 교체할 오디오 코덱 |
| `NVT_AUDIO_BITRATE` | `384k` | 오디오 비트레이트 (무손실 코덱에서는 무시) |
| `NVT_AUDIO_CHANNELS` | `0` | `0`은 원본 채널 유지, `2`는 스테레오 다운믹스 |
| `NVT_VIDEO_CODEC` | `libx264` | `video` 플랜에서만 사용 |
| `NVT_VIDEO_PRESET` | `veryfast` | |
| `NVT_VIDEO_CRF` | `23` | |
| `NVT_CACHE_MAX_GB` | `100` | LRU 제거 기준 |
| `NVT_TRANSCODE_JOBS` | `1` | 동시 ffmpeg 작업 수 (오디오 전용이면 2 이상 권장) |
| `NVT_PROBE_WORKERS` | `6` | 동시 ffprobe 호출 수 |
| `NVT_LOG_REQUESTS` | `true` | GET·HEAD 요청과 Range 헤더를 로그에 남김 |
| `NVT_PREFETCH` | `false` | 폴더를 열 때 그 안의 파일을 미리 변환 |
| `NVT_PREFETCH_MAX` | `3` | 한 폴더에서 미리 변환할 최대 개수 |
| `NVT_PREFETCH_VIDEO` | `false` | 영상 재인코딩까지 추측으로 시작할지 |
| `NVT_WAIT_COMPLETE` | `true` | 변환이 끝날 때까지 GET을 대기 |
| `NVT_WAIT_TIMEOUT_SEC` | `1800` | 초과 시 점진적 스트리밍으로 전환 (시크 불가) |

`NVT_AUDIO_CHANNELS=0`은 5.1을 멀티채널 AAC로 유지하며, Apple TV가 이를
지원합니다. 중간의 리시버가 문제를 일으키면 `2`로 바꾸세요. 다만 스테레오
다운믹스는 센터 채널을 접어 넣으므로 **대사가 작아지는 것을 감수**해야 합니다.

## 재생이 실제로 이루어지는 과정

1. 플레이어가 폴더에 `PROPFIND`를 보냅니다. 그 안의 모든 미디어 파일을
   검사하고(병렬 처리, 캐시됨), 변환이 필요한 것은 `.mkv`로 표시합니다.
   **탐색만으로는 아무것도 변환하지 않습니다.**
2. 플레이어가 `GET`을 보냅니다. 이 작업은 **재생 우선순위**로 큐에 들어가
   추측성 작업을 앞지르며, 필요하면 실행 중인 프리페치를 중단시킵니다.
   `HEAD`는 재생이 아니므로 아무것도 시작하지 않습니다 — 변환 전이라면
   크기를 모르므로 `Content-Length` 없이 답합니다.
3. 동시에 **같은 폴더의 다음 파일 하나**가 예약됩니다. 다음 화를 이어 볼
   확률이 높기 때문입니다. 예약은 이 한 개뿐입니다.
4. 변환이 끝나 있으면 평범한 정적 파일로 서빙됩니다 — 정확한
   `Content-Length`, 완전한 시크, 이어받기 모두 정상입니다.
5. 아직 안 끝났으면 `NVT_WAIT_TIMEOUT_SEC`만큼 기다립니다.
6. 그래도 안 끝나면 커지는 중인 파일을 점진적으로 스트리밍합니다.

다른 폴더의 파일을 재생하면, 앞서 예약해둔 다음 파일은 버려집니다.

`NVT_PREFETCH=true` 로 켜면 폴더를 열 때 그 안의 파일을 `NVT_PREFETCH_MAX`개까지
미리 변환합니다. 기본값이 꺼짐인 이유는, 플레이어가 라이브러리를 만들려고 공유
폴더 전체를 훑기 때문입니다 — 켜두면 결국 라이브러리 전부를 변환하게 됩니다.

### 알려진 한계

**6번은 시크를 포기합니다.** 아직 쓰이는 중인 파일은 최종 크기를 알 수 없어
`Content-Length` 없이(chunked) 전송하기 때문입니다. 크기를 추측해서 보내지는
않습니다 — 낮게 잡으면 재생이 중간에 잘리고 높게 잡으면 오지 않는 바이트를
기다리며 멈춥니다. FLAC 출력은 원본보다 크므로 "원본 크기"라는 그럴듯한
추측은 항상 낮은 쪽으로 틀립니다.

재생 자체는 끝까지 정상입니다. 인코딩이 재생보다 수십 배 빠르므로 재생이
변환을 따라잡지도 않습니다. 그리고 변환이 끝난 뒤의 모든 재생은 4번 경로를
타므로 시크가 완전히 동작합니다.

이 경로를 아예 안 타게 하려면 `NVT_WAIT_TIMEOUT_SEC`을 변환 시간보다 넉넉히
크게 잡으세요 (예: `1800`). 그러면 항상 완성된 파일을 받게 됩니다.

## 상태 확인

```bash
curl -s http://<나스주소>:8080/__nvt/status | jq
```

캐시 사용량과 진행 중인 변환 작업을 보여줍니다.

## 구조

```
cmd/nvt          진입점, HTTP 배선, 인증
internal/config  환경변수 설정
internal/probe   ffprobe 및 passthrough/audio/video 판정, 디스크 캐시
internal/cache   변환 결과물, 완료 마커, LRU 제거
internal/transcode  ffmpeg 작업. 캐시 키로 중복 제거하고 동시 실행 수를 제한
internal/vfs     WebDAV 파일시스템. 이름 매핑과 파일 핸들
```

## 아직 안 한 것

- 배치 모드(라이브러리를 미리 변환해 두고 프록시를 아예 걷어내는 방식).
  `probe` + `transcode` 위에 얹으면 되며, 필요한 배관은 이미 다 있습니다.
- 변환 진행 중인 파일의 정확한 `Content-Length`.
