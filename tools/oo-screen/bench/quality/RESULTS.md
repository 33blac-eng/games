# bench/quality — Stage 0 results (SIMULATION)

> **SIMULATION, NOT MEASURED ON REAL HARDWARE.** Synthetic corpus (`bench/quality/gen_corpus.py`), pipeline emulated in numpy + ffmpeg libx264 on Linux. No Windows capture, no MediaFoundation MFT, no browser decode/compositor. Numbers are for relative comparison of variants only.

## Setup

- Colour: RGB -> Y'CbCr BT.709 limited range (16-235/240), 8-bit; 4:2:0 = 2x2 box average, receiver bilinear chroma upsample; 4:4:4 = no subsampling.
- Scale: `scale0.75` = agent downscale x0.75 (bicubic), viewer bilinear upscale back to native size.
- Codec: ffmpeg libx264, preset veryfast, tune zerolatency, CBR-ish (b=maxrate=bufsize), 30 identical frames @30fps, last decoded frame scored. 4:2:0 = profile main; 4:4:4 uses high444 (Main cannot carry 4:4:4 and WebRTC/Chrome won't accept it — reference only).
- VMAF: **n/a** — this ffmpeg build has no libvmaf filter (only vmafmotion).
- Text sharpness: mask = pixels whose 3x3 luma range in the original > 64, dilated 1px. edge-SSIM = SSIM of gradient-magnitude maps averaged inside mask; grad-energy ratio = sum|grad dist|^2 / sum|grad ref|^2 inside mask (1 = as sharp, <1 blurred).
- `raw` = colour/scale chain only, no codec.

## Mean over corpus per variant

| n | variant | PSNR dB | chroma PSNR dB | SSIM | VMAF | text edge-SSIM | text grad-energy ratio | kbit/frame |
|---|---|---|---|---|---|---|---|---|
| 8 | 444 raw | 65.90 | 74.07 | 1.0000 | n/a | 1.0000 | 1.000 | n/a |
| 8 | 420 raw | 34.75 | 40.32 | 0.9998 | n/a | 0.9993 | 0.981 | n/a |
| 8 | 420 scale0.75 raw | 25.03 | 38.54 | 0.9277 | n/a | 0.6146 | 0.331 | n/a |
| 8 | 420 4M | 32.36 | 39.29 | 0.9900 | n/a | 0.9931 | 0.958 | 100 |
| 8 | 420 scale0.75 4M | 24.98 | 38.17 | 0.9242 | n/a | 0.6122 | 0.329 | 93 |
| 8 | 444 4M | 38.83 | 46.69 | 0.9893 | n/a | 0.9923 | 0.963 | 109 |
| 8 | 444 scale0.75 4M | 25.45 | 40.69 | 0.9242 | n/a | 0.6171 | 0.332 | 103 |
| 8 | 420 8M | 34.19 | 39.96 | 0.9989 | n/a | 0.9987 | 0.977 | 157 |
| 8 | 420 scale0.75 8M | 25.02 | 38.46 | 0.9277 | n/a | 0.6143 | 0.330 | 141 |
| 8 | 444 8M | 46.81 | 53.49 | 0.9990 | n/a | 0.9992 | 0.990 | 177 |
| 8 | 444 scale0.75 8M | 25.56 | 41.53 | 0.9285 | n/a | 0.6213 | 0.335 | 163 |

## Per image

| image | variant | PSNR dB | chroma PSNR dB | SSIM | VMAF | text edge-SSIM | text grad-energy ratio | kbit/frame |
|---|---|---|---|---|---|---|---|---|
| code-dark-1080p.png | 444 raw | 67.72 | 73.41 | 1.0000 | n/a | 1.0000 | 1.000 | n/a |
| code-dark-1080p.png | 420 raw | 38.27 | 42.43 | 1.0000 | n/a | 0.9999 | 0.997 | n/a |
| code-dark-1080p.png | 420 scale0.75 raw | 30.52 | 41.19 | 0.9719 | n/a | 0.6518 | 0.353 | n/a |
| code-dark-1080p.png | 420 4M | 38.11 | 42.33 | 0.9996 | n/a | 0.9989 | 0.997 | 72 |
| code-dark-1080p.png | 420 scale0.75 4M | 30.52 | 41.16 | 0.9717 | n/a | 0.6516 | 0.353 | 69 |
| code-dark-1080p.png | 444 4M | 46.99 | 52.02 | 0.9995 | n/a | 0.9985 | 0.998 | 87 |
| code-dark-1080p.png | 444 scale0.75 4M | 31.09 | 44.70 | 0.9716 | n/a | 0.6512 | 0.353 | 84 |
| code-dark-1080p.png | 420 8M | 38.23 | 42.41 | 0.9999 | n/a | 0.9996 | 0.997 | 97 |
| code-dark-1080p.png | 420 scale0.75 8M | 30.52 | 41.18 | 0.9718 | n/a | 0.6516 | 0.353 | 74 |
| code-dark-1080p.png | 444 8M | 53.49 | 58.60 | 0.9999 | n/a | 0.9996 | 0.998 | 131 |
| code-dark-1080p.png | 444 scale0.75 8M | 31.11 | 44.90 | 0.9718 | n/a | 0.6515 | 0.353 | 107 |
| code-dark-1440p.png | 444 raw | 69.06 | 74.75 | 1.0000 | n/a | 1.0000 | 1.000 | n/a |
| code-dark-1440p.png | 420 raw | 39.65 | 43.81 | 1.0000 | n/a | 0.9999 | 0.997 | n/a |
| code-dark-1440p.png | 420 scale0.75 raw | 31.83 | 42.56 | 0.9793 | n/a | 0.6516 | 0.353 | n/a |
| code-dark-1440p.png | 420 4M | 39.11 | 43.45 | 0.9993 | n/a | 0.9971 | 0.996 | 73 |
| code-dark-1440p.png | 420 scale0.75 4M | 31.81 | 42.48 | 0.9789 | n/a | 0.6505 | 0.353 | 65 |
| code-dark-1440p.png | 444 4M | 44.22 | 49.21 | 0.9991 | n/a | 0.9964 | 0.996 | 86 |
| code-dark-1440p.png | 444 scale0.75 4M | 32.34 | 45.63 | 0.9789 | n/a | 0.6501 | 0.353 | 81 |
| code-dark-1440p.png | 420 8M | 39.50 | 43.72 | 0.9997 | n/a | 0.9991 | 0.997 | 93 |
| code-dark-1440p.png | 420 scale0.75 8M | 31.83 | 42.54 | 0.9792 | n/a | 0.6513 | 0.353 | 93 |
| code-dark-1440p.png | 444 8M | 50.66 | 55.72 | 0.9997 | n/a | 0.9991 | 0.997 | 126 |
| code-dark-1440p.png | 444 scale0.75 8M | 32.40 | 46.20 | 0.9792 | n/a | 0.6511 | 0.353 | 125 |
| code-light-1080p.png | 444 raw | 68.06 | 73.84 | 1.0000 | n/a | 1.0000 | 0.999 | n/a |
| code-light-1080p.png | 420 raw | 31.84 | 38.16 | 0.9999 | n/a | 0.9992 | 0.973 | n/a |
| code-light-1080p.png | 420 scale0.75 raw | 26.20 | 36.13 | 0.9659 | n/a | 0.6074 | 0.325 | n/a |
| code-light-1080p.png | 420 4M | 31.83 | 38.10 | 0.9997 | n/a | 0.9990 | 0.971 | 74 |
| code-light-1080p.png | 420 scale0.75 4M | 26.19 | 36.12 | 0.9659 | n/a | 0.6074 | 0.324 | 67 |
| code-light-1080p.png | 444 4M | 47.42 | 52.82 | 0.9998 | n/a | 0.9995 | 0.989 | 86 |
| code-light-1080p.png | 444 scale0.75 4M | 26.98 | 39.12 | 0.9666 | n/a | 0.6173 | 0.332 | 80 |
| code-light-1080p.png | 420 8M | 31.84 | 38.15 | 0.9998 | n/a | 0.9992 | 0.972 | 95 |
| code-light-1080p.png | 420 scale0.75 8M | 26.19 | 36.13 | 0.9659 | n/a | 0.6074 | 0.324 | 78 |
| code-light-1080p.png | 444 8M | 54.75 | 60.29 | 0.9999 | n/a | 0.9999 | 0.995 | 125 |
| code-light-1080p.png | 444 scale0.75 8M | 27.01 | 39.21 | 0.9667 | n/a | 0.6178 | 0.332 | 101 |
| code-light-1440p.png | 444 raw | 69.37 | 75.17 | 1.0000 | n/a | 1.0000 | 0.999 | n/a |
| code-light-1440p.png | 420 raw | 33.23 | 39.47 | 0.9999 | n/a | 0.9992 | 0.973 | n/a |
| code-light-1440p.png | 420 scale0.75 raw | 27.51 | 37.44 | 0.9749 | n/a | 0.6074 | 0.325 | n/a |
| code-light-1440p.png | 420 4M | 33.14 | 39.26 | 0.9996 | n/a | 0.9985 | 0.969 | 78 |
| code-light-1440p.png | 420 scale0.75 4M | 27.50 | 37.41 | 0.9748 | n/a | 0.6075 | 0.324 | 69 |
| code-light-1440p.png | 444 4M | 44.28 | 49.65 | 0.9996 | n/a | 0.9987 | 0.982 | 88 |
| code-light-1440p.png | 444 scale0.75 4M | 28.24 | 40.26 | 0.9753 | n/a | 0.6164 | 0.331 | 82 |
| code-light-1440p.png | 420 8M | 33.22 | 39.42 | 0.9998 | n/a | 0.9990 | 0.971 | 99 |
| code-light-1440p.png | 420 scale0.75 8M | 27.51 | 37.44 | 0.9749 | n/a | 0.6074 | 0.324 | 89 |
| code-light-1440p.png | 444 8M | 51.34 | 56.77 | 0.9999 | n/a | 0.9997 | 0.992 | 126 |
| code-light-1440p.png | 444 scale0.75 8M | 28.31 | 40.50 | 0.9755 | n/a | 0.6176 | 0.332 | 118 |
| colortext-1080p.png | 444 raw | 62.95 | 68.50 | 1.0000 | n/a | 1.0000 | 1.000 | n/a |
| colortext-1080p.png | 420 raw | 27.88 | 32.30 | 0.9994 | n/a | 0.9982 | 0.956 | n/a |
| colortext-1080p.png | 420 scale0.75 raw | 22.11 | 30.46 | 0.9187 | n/a | 0.5778 | 0.310 | n/a |
| colortext-1080p.png | 420 4M | 27.52 | 32.00 | 0.9979 | n/a | 0.9963 | 0.951 | 109 |
| colortext-1080p.png | 420 scale0.75 4M | 22.09 | 30.44 | 0.9183 | n/a | 0.5779 | 0.310 | 102 |
| colortext-1080p.png | 444 4M | 33.68 | 39.06 | 0.9975 | n/a | 0.9962 | 0.965 | 115 |
| colortext-1080p.png | 444 scale0.75 4M | 22.71 | 33.10 | 0.9204 | n/a | 0.5912 | 0.319 | 111 |
| colortext-1080p.png | 420 8M | 27.87 | 32.30 | 0.9991 | n/a | 0.9981 | 0.956 | 188 |
| colortext-1080p.png | 420 scale0.75 8M | 22.10 | 30.46 | 0.9186 | n/a | 0.5778 | 0.310 | 162 |
| colortext-1080p.png | 444 8M | 42.17 | 47.52 | 0.9995 | n/a | 0.9995 | 0.987 | 208 |
| colortext-1080p.png | 444 scale0.75 8M | 22.86 | 33.49 | 0.9216 | n/a | 0.5953 | 0.323 | 202 |
| colortext-1440p.png | 444 raw | 64.22 | 69.78 | 1.0000 | n/a | 1.0000 | 1.000 | n/a |
| colortext-1440p.png | 420 raw | 29.16 | 33.58 | 0.9995 | n/a | 0.9982 | 0.956 | n/a |
| colortext-1440p.png | 420 scale0.75 raw | 23.37 | 31.75 | 0.9393 | n/a | 0.5762 | 0.309 | n/a |
| colortext-1440p.png | 420 4M | 28.10 | 32.72 | 0.9964 | n/a | 0.9910 | 0.937 | 107 |
| colortext-1440p.png | 420 scale0.75 4M | 23.31 | 31.63 | 0.9385 | n/a | 0.5746 | 0.307 | 103 |
| colortext-1440p.png | 444 4M | 31.92 | 37.57 | 0.9952 | n/a | 0.9881 | 0.938 | 114 |
| colortext-1440p.png | 444 scale0.75 4M | 23.82 | 33.84 | 0.9393 | n/a | 0.5835 | 0.313 | 109 |
| colortext-1440p.png | 420 8M | 29.08 | 33.52 | 0.9991 | n/a | 0.9977 | 0.955 | 194 |
| colortext-1440p.png | 420 scale0.75 8M | 23.37 | 31.75 | 0.9392 | n/a | 0.5763 | 0.309 | 182 |
| colortext-1440p.png | 444 8M | 38.75 | 44.03 | 0.9992 | n/a | 0.9987 | 0.978 | 210 |
| colortext-1440p.png | 444 scale0.75 8M | 24.07 | 34.66 | 0.9411 | n/a | 0.5921 | 0.320 | 201 |
| sheet-1080p.png | 444 raw | 62.66 | 78.81 | 1.0000 | n/a | 1.0000 | 1.000 | n/a |
| sheet-1080p.png | 420 raw | 39.25 | 46.70 | 0.9999 | n/a | 0.9999 | 0.998 | n/a |
| sheet-1080p.png | 420 scale0.75 raw | 19.36 | 44.67 | 0.8363 | n/a | 0.6222 | 0.336 | n/a |
| sheet-1080p.png | 420 4M | 33.98 | 44.22 | 0.9915 | n/a | 0.9956 | 0.969 | 133 |
| sheet-1080p.png | 420 scale0.75 4M | 19.33 | 43.82 | 0.8351 | n/a | 0.6202 | 0.333 | 122 |
| sheet-1080p.png | 444 4M | 34.93 | 48.02 | 0.9901 | n/a | 0.9954 | 0.968 | 136 |
| sheet-1080p.png | 444 scale0.75 4M | 19.35 | 45.49 | 0.8345 | n/a | 0.6208 | 0.333 | 123 |
| sheet-1080p.png | 420 8M | 38.77 | 46.30 | 0.9995 | n/a | 0.9997 | 0.993 | 226 |
| sheet-1080p.png | 420 scale0.75 8M | 19.36 | 44.61 | 0.8369 | n/a | 0.6219 | 0.335 | 209 |
| sheet-1080p.png | 444 8M | 46.52 | 56.69 | 0.9994 | n/a | 0.9997 | 0.994 | 226 |
| sheet-1080p.png | 444 scale0.75 8M | 19.39 | 47.80 | 0.8373 | n/a | 0.6231 | 0.336 | 213 |
| sheet-1440p.png | 444 raw | 63.17 | 78.28 | 1.0000 | n/a | 1.0000 | 1.000 | n/a |
| sheet-1440p.png | 420 raw | 38.68 | 46.12 | 0.9999 | n/a | 0.9999 | 0.998 | n/a |
| sheet-1440p.png | 420 scale0.75 raw | 19.30 | 44.09 | 0.8351 | n/a | 0.6226 | 0.336 | n/a |
| sheet-1440p.png | 420 4M | 27.12 | 42.20 | 0.9357 | n/a | 0.9682 | 0.874 | 154 |
| sheet-1440p.png | 420 scale0.75 4M | 19.10 | 42.30 | 0.8105 | n/a | 0.6083 | 0.326 | 150 |
| sheet-1440p.png | 444 4M | 27.19 | 45.18 | 0.9335 | n/a | 0.9660 | 0.869 | 156 |
| sheet-1440p.png | 444 scale0.75 4M | 19.10 | 43.33 | 0.8073 | n/a | 0.6064 | 0.325 | 149 |
| sheet-1440p.png | 420 8M | 34.99 | 43.86 | 0.9945 | n/a | 0.9973 | 0.978 | 261 |
| sheet-1440p.png | 420 scale0.75 8M | 19.28 | 43.56 | 0.8350 | n/a | 0.6208 | 0.333 | 239 |
| sheet-1440p.png | 444 8M | 36.83 | 48.31 | 0.9944 | n/a | 0.9972 | 0.977 | 265 |
| sheet-1440p.png | 444 scale0.75 8M | 19.30 | 45.49 | 0.8348 | n/a | 0.6217 | 0.334 | 240 |
