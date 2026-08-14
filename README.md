# vtrans

Re-encodes a video library to AV1 with hardware acceleration, and tells you the
truth about what it did.

A single Go binary with an embedded web interface. No runtime dependencies
beyond `ffmpeg`, no database, no container, no configuration language. Copy the
binary and run it.

On the library it was built for: **1253 files, 2.42 TB → 0.55 TB (77% saved).**

---

## Why another transcoder

Because the existing ones tell you a file failed, not why.

Every diagnostic rule in vtrans came from a real failure where the error message
told the wrong story. A few, with the measurement that settled them:

| The error said | It was actually |
|---|---|
| `ffmpeg was killed — the system likely ran out of memory` | A `mov_text` subtitle the Matroska muxer refuses. ffmpeg 8.0.1 then segfaults while tearing down. Zero OOM records in the kernel log; 10 GB of memory free. |
| `error while decoding` on 39 healthy files | The stderr was shortened to 300 characters *before* being classified, cutting the last line in half. Half a line matched no known-benign pattern, so a clean file was rejected. |
| `No support for codec mpeg4` | The GPU has no MPEG-4 decoder unit. Not a missing codec — ffmpeg decodes it perfectly in software. |
| A queue of 49 that never went down | 43 of them carried permanent failure records and were skipped silently, while still being counted. |

None of these were right on the first guess. Each one is now a named rule with
a comment explaining what was measured, and a test that fails if it regresses.

## What it does

- **Encodes** every video stream, keeps every audio and subtitle track, keeps
  cover art and chapters. Dual-language files stay dual-language.
- **Verifies** before replacing anything: duration within tolerance, stream
  counts match, and the output actually decodes. Only then is the original
  moved to the trash directory.
- **Converts subtitles Matroska cannot store** (`mov_text` → SubRip) instead of
  dropping them, and refuses files whose subtitles ffmpeg cannot read at all
  rather than losing them quietly.
- **Learns your hardware.** Nothing about GPU decode support is hardcoded: it
  tries hardware, and if setup fails it records that codec as software-only and
  retries. Software decode still ends in a hardware encode.
- **Diagnoses.** A `Doctor` view runs `ffprobe` across the whole library and
  reports what would stand in the way of processing each file, in words rather
  than exit codes.

## The interface

`vtrans serve` puts everything on one page: live progress, the library as a
card grid with search and filters, the queue, history, failures, the ignore
list, the service log, and settings.

Artwork already beside your files (`folder.jpg`, `-thumb.jpg`, season posters)
is used as-is, scaled and cached. Nothing is fetched from the network and
nothing is written into your library.

## Requirements

- **Linux** with a **VAAPI** device (`/dev/dri/renderD128`) that can encode AV1.
  In practice: Intel Arc or an Intel iGPU from Meteor Lake onwards, or a recent
  AMD card. `vainfo` should list `VAProfileAV1Profile0: VAEntrypointEncSlice`.
- **ffmpeg 7+** with VAAPI support, on `PATH`.
- Go 1.25+ to build.

**NVIDIA is not supported.** vtrans speaks VAAPI only. NVENC support would be a
welcome contribution.

## Install

```sh
go build -o vtrans .
sudo install -m755 vtrans /usr/local/bin/vtrans
```

First run writes a default configuration you can then edit, either in the
Settings tab or directly:

```sh
vtrans scan          # index the library
vtrans plan          # show what would be processed, change nothing
vtrans run           # process the queue
vtrans serve         # web interface on :7654
```

`vtrans sample <file>` encodes short segments at several quality settings so you
can compare before committing to a value.

### Running it as a service

`packaging/systemd/` has both unit files. Replace `CHANGEME` with your username
and adjust the paths. They are worth reading even if you write your own: the
comments record why each directive is there, including a deadlock that cost a
night to diagnose (`RequiresMountsFor` on an autofs mount with an idle timeout
will stop your service mid-encode).

```sh
sudo cp packaging/systemd/*.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now vtrans vtrans-ui
```

The web interface listens on all interfaces with no authentication, which suits
a home network. On anything less trusted, use `--token <key>` or bind it to
`127.0.0.1`.

## Configuration

`~/.config/vtrans/config.json`, or the Settings tab. The values that matter
most:

| Key | Meaning |
|---|---|
| `mode` | `replace` swaps the original out; `copy` writes elsewhere and leaves it alone |
| `trash_dir` | where replaced originals wait. **Empty means they are deleted outright** |
| `q_movie`, `q_tv` | AV1 quality; higher is smaller. 130 and 150 are good starting points |
| `min_bitrate_mbps` | files below this are left alone — nothing to gain |
| `min_saving_ratio` | outputs that save less than this are rejected and the original kept |
| `skip_on_stream_loss` | refuse files whose streams cannot all be carried over. Leave this on |

## A warning about `replace` mode

It moves your originals to the trash directory and puts the new file in their
place. Verification runs first and is strict, but verification is not the same
as watching the film. Check a few outputs by eye before you empty the trash, and
keep `trash_dir` set until you trust it.

## Support

vtrans is free and always will be. If it saved you a weekend or a few
terabytes, you can say thanks through
[GitHub Sponsors](https://github.com/sponsors/brooqs).

No feature is behind a payment, and none ever will be. Bug reports and hardware
you can test on are worth more than money anyway — particularly NVIDIA, which
nobody has been able to try yet.

## License

MIT. See [LICENSE](LICENSE).

`ffmpeg` is invoked as a separate process and is not linked into this binary,
so its licence does not extend to vtrans. You are responsible for the ffmpeg
build you install.
