#!/usr/bin/env bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 OUTPUT_DIRECTORY [OUTPUT_ARCHIVE.tar.gz]" >&2
  exit 2
fi

command -v ffmpeg >/dev/null || {
  echo "ffmpeg is required to generate the media range test suite" >&2
  exit 1
}

out=$1
archive=${2:-}
media="$out/media"
work="$out/.work"

if [[ -n "$archive" ]]; then
  command -v tar >/dev/null || {
    echo "tar is required to create the media range archive" >&2
    exit 1
  }
  command -v gzip >/dev/null || {
    echo "gzip is required to create the media range archive" >&2
    exit 1
  }
fi

rm -rf "$media" "$work"
mkdir -p "$media" "$work"

cat > "$work/subtitles.ass" <<'EOF'
[Script Info]
Title: Range Probe Test
ScriptType: v4.00+
PlayResX: 320
PlayResY: 180

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,DejaVu Sans,18,&H00FFFFFF,&H000000FF,&H00000000,&H64000000,0,0,0,0,100,100,0,0,1,1,0,2,10,10,12,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:03.50,Default,,0,0,0,,Range probe subtitle
Dialogue: 0,0:00:04.00,0:00:05.50,Default,,0,0,0,,Second subtitle packet
EOF

cat > "$work/subtitles.srt" <<'EOF'
1
00:00:01,000 --> 00:00:03,500
Range probe subtitle

2
00:00:04,000 --> 00:00:05,500
Second subtitle packet
EOF

video='testsrc2=size=320x180:rate=24000/1001:duration=6'
audio='sine=frequency=1000:sample_rate=44100:duration=6'
ffmpeg_common=(-hide_banner -loglevel error -nostdin -y)

ffmpeg "${ffmpeg_common[@]}" \
  -f lavfi -i "$video" \
  -f lavfi -i "$audio" \
  -i "$work/subtitles.ass" \
  -map 0:v:0 -map 1:a:0 -map 2:s:0 \
  -c:v libx264 -preset ultrafast -crf 28 -g 48 -pix_fmt yuv420p \
  -c:a aac -b:a 192k -c:s ass \
  -metadata title='MKV normal front metadata' \
  -metadata:s:v:0 language=jpn -metadata:s:a:0 language=jpn -metadata:s:s:0 language=jpn \
  -disposition:v:0 default -disposition:a:0 default -disposition:s:0 0 \
  -cluster_time_limit 1000 \
  "$media/01_mkv_normal_front.mkv"

ffmpeg "${ffmpeg_common[@]}" \
  -f lavfi -i "$video" \
  -f lavfi -i "$audio" \
  -i "$work/subtitles.ass" \
  -map 0:v:0 -map 1:a:0 -map 2:s:0 \
  -c:v libx264 -preset ultrafast -crf 28 -g 48 -pix_fmt yuv420p \
  -c:a aac -b:a 192k -c:s ass \
  -metadata title='MKV large Void before clusters' \
  -metadata:s:v:0 language=jpn -metadata:s:a:0 language=jpn -metadata:s:s:0 language=jpn \
  -disposition:v:0 default -disposition:a:0 default -disposition:s:0 0 \
  -reserve_index_space 4194304 -cluster_time_limit 1000 \
  "$media/02_mkv_large_void_before_cluster.mkv"

ffmpeg "${ffmpeg_common[@]}" \
  -f lavfi -i "$video" \
  -f lavfi -i "$audio" \
  -i "$work/subtitles.srt" \
  -map 0:v:0 -map 1:a:0 -map 2:s:0 \
  -c:v libx264 -preset ultrafast -crf 28 -g 48 -pix_fmt yuv420p \
  -c:a aac -b:a 192k -c:s mov_text \
  -metadata title='MP4 moov at tail' \
  -metadata:s:v:0 language=jpn -metadata:s:a:0 language=jpn -metadata:s:s:0 language=jpn \
  -disposition:v:0 default -disposition:a:0 default -disposition:s:0 0 \
  "$media/03_mp4_moov_at_end.mp4"

ffmpeg "${ffmpeg_common[@]}" \
  -i "$media/03_mp4_moov_at_end.mp4" -map 0 -c copy -movflags +faststart \
  "$media/04_mp4_faststart.mp4"

ffmpeg "${ffmpeg_common[@]}" \
  -f lavfi -i 'testsrc2=size=320x180:rate=25:duration=10' \
  -itsoffset 6 -f lavfi -i 'sine=frequency=600:sample_rate=48000:duration=4' \
  -map 0:v:0 -map 1:a:0 \
  -c:v libx264 -preset ultrafast -b:v 900k -maxrate 900k -bufsize 1800k -g 50 -pix_fmt yuv420p \
  -c:a aac -b:a 128k \
  -metadata:s:v:0 language=jpn -metadata:s:a:0 language=jpn \
  -muxrate 4000k -mpegts_flags +resend_headers -muxdelay 0 \
  "$media/05_mpegts_audio_starts_late.ts"

ffmpeg "${ffmpeg_common[@]}" \
  -f lavfi -i "$video" \
  -f lavfi -i "$audio" \
  -map 0:v:0 -map 1:a:0 \
  -c:v mpeg4 -q:v 7 -g 48 -c:a libmp3lame -b:a 128k \
  -metadata title='AVI tail index' \
  "$media/06_avi_tail_index.avi"

ffmpeg "${ffmpeg_common[@]}" \
  -f lavfi -i "$video" \
  -f lavfi -i "$audio" \
  -map 0:v:0 -map 1:a:0 \
  -c:v libtheora -q:v 6 -c:a libvorbis -q:a 4 \
  -metadata title='Ogg tail granule duration' \
  -metadata:s:v:0 language=jpn -metadata:s:a:0 language=jpn \
  "$media/07_ogg_theora_vorbis.ogv"

ffmpeg "${ffmpeg_common[@]}" \
  -i "$media/01_mkv_normal_front.mkv" -map 0:v:0 -c copy \
  -bsf:v h264_mp4toannexb -f h264 "$media/08_raw_h264_annexb.h264"

ffmpeg "${ffmpeg_common[@]}" \
  -i "$media/01_mkv_normal_front.mkv" -map 0:a:0 -c copy \
  -f adts "$media/09_raw_aac_adts.aac"

head -c 131072 "$media/03_mp4_moov_at_end.mp4" > "$media/10_truncated_mp4_no_moov.mp4"
head -c 196608 "$media/01_mkv_normal_front.mkv" > "$media/11_truncated_mkv_header_and_packets.mkv"
cp "$media/01_mkv_normal_front.mkv" "$media/12_mkv_with_bin_extension.bin"

rm -rf "$work"

if [[ -n "$archive" ]]; then
  mkdir -p "$(dirname "$archive")"
  archive_tmp="${archive}.tmp"
  rm -f "$archive_tmp"
  tar \
    --sort=name \
    --mtime='UTC 1970-01-01' \
    --owner=0 \
    --group=0 \
    --numeric-owner \
    --format=ustar \
    -C "$out" \
    -cf - media | gzip -n -9 > "$archive_tmp"
  mv "$archive_tmp" "$archive"
fi
