These two-second, 440 Hz synthetic tones are generated test fixtures, with no third-party audio. `tone.mp3` tests single-file conversion; `tone.m4b` contains two one-second chapters to test chapter splitting. Tests serve them from a local mock ABS HTTP server and run real FFmpeg/FFprobe. No ABS account or network access is required after Go dependencies are installed.

Generate the MP3 with:

```sh
ffmpeg -f lavfi -i 'sine=frequency=440:duration=2' -ar 44100 -ac 2 -c:a libmp3lame -b:a 64k tone.mp3
```

To regenerate the M4B, create an FFmetadata file with two chapters (TIMEBASE=1/1000, START/END=0/1000 and 1000/2000), then run:

```sh
ffmpeg -i tone.mp3 -i chapters.txt -map_metadata 1 -map_chapters 1 -c:a aac -b:a 64k tone.m4b
```
