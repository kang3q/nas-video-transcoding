# Apple TV 4K 재생 호환성 — 변환 결과물 검토 기준

v2 가 만들어 내는 MP4 가 Apple TV 4K 에서 실제로 재생되는지 판단하는 기준입니다.
구현이 끝난 뒤 이 문서의 항목을 하나씩 대조하는 용도로 씁니다.

출발점은 아래 형태의 배치 스크립트였습니다. 코덱 조합만 보면 맞지만 이대로는
여러 조건에서 재생이 깨지고 원본이 사라집니다. 무엇이 왜 문제인지를 정리합니다.

```bash
ffmpeg -i "$file" -threads 3 -vcodec libx264 -vsync 2 -preset superfast \
  -vprofile main -level 40 -pix_fmt yuv420p -b:v 2600k \
  -acodec aac -ab 320k -ac 2 -ar 48000 \
  -f mp4 -map_metadata 0 -map 0:0 -map 0:1 -y "${file}.mp4"
rm -f "$file"
mv -f "${file}.mp4" "$file"
```

## 결론

H.264 Main + AAC-LC 2ch 48kHz 라는 조합 자체는 Apple TV 4K 가 확실히 디코딩합니다.
AirPlay 2 비디오는 파일을 Apple TV 로 넘겨 거기서 디코딩시키는 경우가 많으므로,
결국 Apple TV 의 디코더 지원 범위가 그대로 적용됩니다. 문제는 코덱이 아니라
컨테이너, 헤더, 스트림 선택, 그리고 파일 수명 관리 쪽에 있습니다.

## 재생을 막는 항목

### 1. 확장자와 컨테이너 불일치

`-f mp4` 로 만든 뒤 원본 이름으로 되돌리면 `movie.mkv` 라는 이름에 MP4 알맹이가
들어갑니다. Apple 계열은 확장자와 MIME 타입을 상당히 신뢰하고, WebDAV 로 서빙할 때
`Content-Type` 도 함께 틀어집니다.

**검토 항목** — 출력 파일의 확장자가 `.mp4` 인가. 서빙 시 `video/mp4` 로 나가는가.

### 2. `-movflags +faststart` 누락

없으면 moov atom 이 파일 끝에 붙습니다. 로컬 재생은 되지만 네트워크 재생에서는
플레이어가 파일 끝을 먼저 받아야 시작합니다. 10GB 짜리라면 한참 멈춰 있거나
타임아웃납니다. 네트워크 재생이 전제라면 선택이 아니라 필수입니다.

**검토 항목** — ffmpeg 인자에 `-movflags +faststart` 가 있는가.

### 3. 레벨과 해상도 불일치

`-level 40` 은 1080p 까지입니다. 스케일 다운 없이 4K 소스에 적용하면 해상도는
4K 인데 헤더에는 4.0 이라고 적힌 규격 위반 파일이 나옵니다. Apple 디코더는 레벨
검사가 엄격해 이런 파일을 거부합니다.

Apple TV 4K 는 H.264 를 1080p 까지만 지원하고 4K 는 HEVC 로 처리합니다.

**검토 항목** — H.264 로 인코딩할 때 1080p 이하로 스케일하는가
(`-vf "scale='min(1920,iw)':-2"`), 레벨을 4.2 로 두는가.

### 4. HDR 톤매핑 부재

4K HDR 을 톤매핑 없이 8bit SDR 로 내리면 전체가 회색빛으로 물빠진 화면이 됩니다.
재생은 되지만 볼 수 없는 결과물입니다.

Apple TV 4K 는 HEVC HDR 을 네이티브로 재생하므로, 이 경우 **변환하지 않고 그대로
두는 것이 정답**입니다.

**검토 항목** — HDR 소스를 SDR 로 강제 변환하는 경로가 있는가. 있다면 제거 대상.

### 5. 스트림 인덱스 하드코딩

`-map 0:0 -map 0:1` 은 스트림 0/1 이 비디오/오디오라고 가정합니다. MKV 에서는
자막이나 첨부 폰트가 1번인 파일이 흔하고, 그러면 오디오 인코딩이 실패하거나
엉뚱한 트랙이 들어갑니다.

**검토 항목** — `-map 0:v:0 -map 0:a:0` 처럼 종류를 지정하는가. 자막·데이터
스트림을 `-sn -dn` 으로 배제하는가.

## 스크립트 수준의 결함

### 6. ffmpeg 가 stdin 을 소비한다

`find | while read` 안에서 ffmpeg 를 실행하면 ffmpeg 가 stdin 을 읽어 파일 목록을
통째로 삼킵니다. 첫 파일 하나만 처리하고 조용히 끝납니다. `-nostdin` 을 붙이거나
`< /dev/null` 을 리다이렉트해야 합니다.

v2 가 Go 에서 `exec.Command` 로 호출한다면 자연히 해당되지 않지만, 자식 프로세스의
stdin 을 명시적으로 닫아 두는 편이 안전합니다.

### 7. 검증 전 원본 삭제

`rm -f` 다음에 `mv` 하는 순서는 복구가 불가능합니다. ffmpeg 는 스트림 매핑이
어긋난 상태에서도 exit 0 을 내며 깨진 파일을 만드는 경우가 있고, 중간에 중단되면
원본만 사라집니다.

**검토 항목** — 출력을 원본과 다른 경로에 쓰는가. 임시 이름(`.part`)으로 쓴 뒤
성공했을 때만 최종 이름으로 옮기는가. 원본 삭제가 사용자의 명시적 행동인가.

### 8. 무조건 재인코딩

이미 H.264/HEVC 인 파일까지 다시 인코딩하면 화질만 잃고 시간을 버립니다.
Synology CPU 로 4K H.264 인코딩은 영화 한 편에 반나절이 걸립니다.

**검토 항목** — 비디오를 `-c:v copy` 로 넘길 수 있는 조건을 판별하는가.

## 대부분의 파일은 오디오만 바꾸면 된다

Apple TV 4K 는 HEVC, HDR, H.264 대부분을 그대로 재생합니다. 실제로 막히는 것은
보통 오디오입니다 — DTS, TrueHD, EAC3 때문에 "화면은 나오는데 소리가 없는" 상황.
이때는 비디오를 건드리지 않고 오디오만 바꿔 리먹싱하면 끝납니다.

```
-map 0:v:0 -map 0:a:0 -c:v copy -c:a <오디오코덱> -movflags +faststart -sn -dn
```

화질 손실이 없고 CPU 를 거의 쓰지 않습니다. v1 의 `audio` 플랜과 같은 판단이며,
v2 도 이 경로를 기본으로 두어야 합니다.

### 다만 MP4 에서는 FLAC 을 쓸 수 없다

v1 은 저전력 CPU 에서 AAC 인코딩이 느리다는 실측(Celeron J1900 에서 AAC 384k 가
4x, FLAC 이 60x)을 근거로 FLAC 을 골랐습니다. 그 근거 자체는 유효하지만, **FLAC 은
Matroska 라서 가능했던 선택**입니다. v1 은 `.mkv` 로 서빙했고 Infuse 가 이를
재생했습니다.

v2 는 MP4 로 내보냅니다. Apple 은 MP4 안의 FLAC 을 지원하지 않으므로 그대로
가져오면 소리가 나오지 않습니다. MP4 에서 쓸 수 있는 선택지는 둘입니다.

| 코덱 | 특성 | 비고 |
|---|---|---|
| AAC-LC | 손실, 심리음향 모델 때문에 느림 | 호환성은 가장 확실 |
| ALAC | 무손실, 모델이 없어 빠름 | Apple 네이티브, MP4 수납 가능 |

ALAC 은 FLAC 과 같은 이유로 빠르면서 Apple 이 자사 컨테이너에서 지원하는
코덱이므로, v1 이 FLAC 으로 얻었던 속도 이점을 MP4 에서 유지할 후보입니다.
다만 v1 의 표에 있는 수치는 FLAC 기준이므로, **ALAC 의 인코딩 속도와 Apple TV
실기 재생은 별도로 측정해서 확인해야 합니다.** 확인 전에는 AAC 를 기본으로 두는
편이 안전합니다.

용량은 무손실이라 늘어납니다. v1 기준 FLAC 이 스테레오 48kHz 에서 700~900kb/s
였으니 ALAC 도 비슷한 규모를 예상하면 됩니다.

## 참고 구현

위 항목을 모두 반영한 형태입니다. v2 의 변환 파이프라인이 만들어 내는 명령과
대조하는 기준으로 씁니다.

```bash
vcodec=$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name -of csv=p=0 "$file")
height=$(ffprobe -v error -select_streams v:0 -show_entries stream=height   -of csv=p=0 "$file")

if { [ "$vcodec" = "h264" ] || [ "$vcodec" = "hevc" ]; } && [ "${height:-0}" -le 2160 ]; then
  vopts=(-c:v copy)
else
  vopts=(-c:v libx264 -preset superfast -crf 20 -profile:v high -level 4.2
         -pix_fmt yuv420p -vf "scale='min(1920,iw)':-2")
fi

ffmpeg -nostdin -v error -stats -i "$file" \
  -map 0:v:0 -map 0:a:0 -sn -dn \
  "${vopts[@]}" \
  -c:a aac -b:a 256k -ac 2 -ar 48000 \
  -movflags +faststart -map_metadata 0 \
  -y "$out.part" && mv -f "$out.part" "$out" || rm -f "$out.part"
```

## 검토 체크리스트

- [ ] 출력 확장자가 `.mp4` 이고 서빙 MIME 이 `video/mp4` 인가
- [ ] `-movflags +faststart` 가 붙는가
- [ ] H.264 인코딩 시 1080p 이하로 스케일하고 레벨이 4.2 인가
- [ ] HDR 소스를 SDR 로 강제 변환하지 않는가
- [ ] 스트림을 `-map 0:v:0 -map 0:a:0` 로 지정하고 `-sn -dn` 을 주는가
- [ ] 재인코딩이 불필요한 비디오를 `-c:v copy` 로 넘기는가
- [ ] MP4 출력에 FLAC 을 쓰고 있지 않은가
- [ ] ALAC 을 쓴다면 속도와 실기 재생을 측정했는가
- [ ] 출력이 원본과 분리된 경로에 임시 이름으로 쓰인 뒤 성공 시에만 확정되는가
- [ ] 원본 삭제가 자동이 아니라 사용자의 명시적 행동인가
- [ ] ffmpeg 자식 프로세스의 stdin 이 닫혀 있는가

## 반영 결과 (v2)

이 문서를 기준으로 v2 의 변환 파이프라인을 대조한 결과입니다.

| 항목 | 결과 |
|---|---|
| 출력 확장자 `.mp4`, MIME `video/mp4` | 충족 |
| `-movflags +faststart` | **결함이었음.** 리먹스 경로에만 있었고 재인코딩 경로에 없었습니다. 두 경로 모두에 추가 |
| 1080p 스케일 + 레벨 | **결함이었음.** `-level 4.0` 고정에 스케일 없음. `-profile:v high -level 4.2` 로 바꾸고 1920 초과 시 `scale='min(1920,iw)':-2` 적용 |
| `-sn -dn` | 명시적 `-map` 으로 이미 배제되지만 안전장치로 추가 |
| `-c:v copy` 판별 | 충족 (`RemuxOnly`) |
| MP4 에 FLAC 없음 | 충족. v2 는 AAC 고정이며 v1 의 FLAC 선택을 가져오지 않았습니다 |
| `.part` → 성공 시 rename, 분리된 출력 트리 | 충족 |
| 원본 삭제 없음 | 충족 |
| `-nostdin` | 충족 |

### HEVC / HDR — 문서 권장안과 다르게 결정

이 문서는 Apple TV 4K 가 HEVC/HDR 을 네이티브로 재생하므로 **변환하지 않는 것이
정답**이라고 봅니다. 일반론으로는 맞습니다. 그런데 이 라이브러리에서는 두 가지와
충돌합니다.

- 문제의 발단이 된 `Future Boy Conan - 24 [1080p] [x265] [10bit].mkv` 는 HEVC
  Main10 + HE-AAC 이고 Apple TV 4K 인데도 **Infuse 에서 재생되지 않았습니다.**
  원인은 지금도 확인되지 않았습니다.
- v2 는 브라우저 재생도 목표인데, 브라우저는 HEVC 를 재생하지 못합니다.

그래서 **HEVC 는 H.264 로 재인코딩**하기로 했습니다. 편당 40분과 HDR 손실을
치르는 대신, 원인 불명의 재생 실패를 확실히 우회하고 어느 기기에서든 재생됩니다.
원인이 밝혀지면 다시 판단할 대목입니다.

### ALAC

문서가 후보로 제시한 ALAC 은 채택하지 않았습니다. 문서 자신이 "속도와 실기 재생을
별도로 측정하기 전에는 AAC 가 안전하다"고 적고 있고, 측정한 바가 없습니다.

## 검증 방법

변환된 파일 하나를 실기에서 확인하기 전에 헤더부터 봅니다.

```bash
# 컨테이너, 코덱, 프로파일/레벨, 해상도
ffprobe -v error -show_entries stream=codec_name,profile,level,width,height,codec_type \
  -of default=noprint_wrappers=1 out.mp4

# faststart 여부 — moov 가 mdat 보다 앞에 나와야 한다
ffprobe -v trace -i out.mp4 2>&1 | grep -m2 -E "type:'(moov|mdat)'"
```

그다음 Apple TV 실기에서 영화 두어 편으로 확인하고 전체에 적용합니다.
