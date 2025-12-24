#!/usr/bin/env bash
set -euo pipefail

URL="${1:-ws://localhost:8080/ws?raw=1}"

# Realtime 這裡你已經驗證過要用 24000
RATE="${RATE:-24000}"
CH="${CH:-1}"
REC_SEC="${REC_SEC:-5}"

# 送完音訊後，最多等回覆幾秒（你之前聽不到多半是這個太短）
CAPTURE_SEC="${CAPTURE_SEC:-20}"

UPLOADS_DIR="${UPLOADS_DIR:-uploads}"
mkdir -p "$UPLOADS_DIR"

# random name（mac 有 uuidgen）
RID="$(uuidgen | tr '[:upper:]' '[:lower:]')"
IN_PCM="${UPLOADS_DIR}/in_${RID}_${RATE}hz.pcm"
OUT_PCM="${UPLOADS_DIR}/out_${RID}_${RATE}hz.pcm"

# ---- 選麥克風：預設偏好 Built-in Microphone，避免 iPhone ----
# 你也可以自己指定，例如：
#   AUDIO_DEVICE_NAME="MacBook Air Microphone" ./test_realtime_mac.sh
AUDIO_DEVICE_NAME="${AUDIO_DEVICE_NAME:-}"

pick_audio_device_index() {
  # 取得 avfoundation 裝置清單
  local list
  list="$(ffmpeg -hide_banner -f avfoundation -list_devices true -i "" 2>&1 || true)"

  # 如果使用者指定裝置名稱，優先用它
  if [[ -n "$AUDIO_DEVICE_NAME" ]]; then
    # 找出對應的 index（在 audio devices 區段）
    # 格式通常像： [0] MacBook Air Microphone
    local idx
    idx="$(echo "$list" | awk '
      BEGIN{ina=0}
      /AVFoundation audio devices/ {ina=1; next}
      /AVFoundation video devices/ {ina=0}
      ina==1 {print}
    ' | grep -iF "$AUDIO_DEVICE_NAME" | sed -n 's/.*\[\([0-9]\+\)\].*/\1/p' | head -n 1)"

    if [[ -n "$idx" ]]; then
      echo "$idx"
      return 0
    fi

    echo "❗找不到 AUDIO_DEVICE_NAME=$AUDIO_DEVICE_NAME，將改用自動挑選 Built-in Microphone。" >&2
  fi

  # 自動挑：優先 Built-in Microphone / MacBook* Microphone，且排除 iPhone
  local idx
  idx="$(echo "$list" | awk '
    BEGIN{ina=0}
    /AVFoundation audio devices/ {ina=1; next}
    /AVFoundation video devices/ {ina=0}
    ina==1 {print}
  ' | grep -Ei 'Built-in Microphone|MacBook.*Microphone' | grep -Evi 'iphone|continuity' \
    | sed -n 's/.*\[\([0-9]\+\)\].*/\1/p' | head -n 1)"

  # 找不到就退回 0
  if [[ -z "$idx" ]]; then
    idx="0"
  fi
  echo "$idx"
}

AUDIO_IDX="$(pick_audio_device_index)"

echo "Using audio input device index: $AUDIO_IDX"
echo "Saving input to:  $IN_PCM"
echo "Saving output to: $OUT_PCM"
echo "[start recording] (${REC_SEC}s, ${RATE}Hz, PCM16, mono)"

# avfoundation input 格式： "videoIndex:audioIndex"
# 我們不要 video，videoIndex 用 "none"
ffmpeg -hide_banner -loglevel error \
  -f avfoundation -i "none:${AUDIO_IDX}" \
  -t "$REC_SEC" -ar "$RATE" -ac "$CH" \
  -f s16le -acodec pcm_s16le \
  "$IN_PCM"

echo "[end recording]"
echo "Sending audio to $URL and waiting up to ${CAPTURE_SEC}s for reply..."

# websocat：
# -b binary
# -n stdin EOF 不送 close，保持連線收回覆
# 用 gtimeout 控制總等待時間，避免掛死
gtimeout "$((REC_SEC + CAPTURE_SEC))" \
  bash -c "websocat -b -n '$URL' < '$IN_PCM' > '$OUT_PCM'"

SIZE="$(wc -c < "$OUT_PCM" | tr -d ' ')"
if [[ "$SIZE" -le 0 ]]; then
  echo "❌ No audio received. 請看你的 Go server log 是否有印出 openai error event。"
  exit 1
fi

echo "✅ Received ${SIZE} bytes of PCM. Playing now..."

# 直接播放 raw PCM（不用轉 wav）
OUT_WAV="${OUT_PCM%.pcm}.wav"

echo "Wrapping PCM to WAV: $OUT_WAV"
ffmpeg -hide_banner -loglevel error \
  -f s16le -ar "$RATE" -ac "$CH" -i "$OUT_PCM" \
  -c:a pcm_s16le "$OUT_WAV"

echo "Playing with afplay..."
afplay "$OUT_WAV"

echo "Done. Files saved under: $UPLOADS_DIR"
