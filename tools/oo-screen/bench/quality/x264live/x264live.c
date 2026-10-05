// x264live — ОДИН безперервний енкод x264 зі зміною ставки ПОСЕРЕДИНІ потоку
// (x264_encoder_reconfig), як це робить агент, коли contentmode.Capper міняє
// ціль. Закриває прогалину lowmotion_run.py, де байти кадру бралися з окремих
// енкодів на кожній ставці, а перехідний процес (VBV, референси) не
// моделювався.
//
// stdin  — сирі кадри yuv420p W×H;
// sched  — по рядку на вхідний кадр: "<kbps> <send>"; send=0 — кадр не
//          змінився, агент його не кодує (skip);
// stdout — по рядку на вхідний кадр: байти NAL-ів, що вийшли з енкодера на
//          цьому кадрі (0 для пропущених).
// Параметри — як enc_cmd у workloads_run.py: veryfast, zerolatency, main,
// ABR з VBV maxrate = 1.5×, bufsize = maxrate/2, без періодичних IDR.
//
// Збірка: gcc -O2 -o x264live x264live.c -lx264   (потрібен libx264-dev)
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <x264.h>

static void set_rate(x264_param_t *p, int kbps) {
	p->rc.i_rc_method = X264_RC_ABR;
	p->rc.i_bitrate = kbps;
	p->rc.i_vbv_max_bitrate = kbps * 3 / 2;
	p->rc.i_vbv_buffer_size = kbps * 3 / 4;
}

int main(int argc, char **argv) {
	if (argc != 5) {
		fprintf(stderr, "usage: x264live W H FPS schedule.txt < frames.yuv\n");
		return 2;
	}
	int w = atoi(argv[1]), h = atoi(argv[2]), fps = atoi(argv[3]);
	FILE *sf = fopen(argv[4], "r");
	if (!sf) {
		perror("schedule");
		return 1;
	}
	x264_param_t p;
	if (x264_param_default_preset(&p, "veryfast", "zerolatency") < 0)
		return 1;
	p.i_width = w;
	p.i_height = h;
	p.i_csp = X264_CSP_I420;
	p.i_fps_num = fps;
	p.i_fps_den = 1;
	p.i_timebase_num = 1;
	p.i_timebase_den = fps;
	p.i_keyint_max = X264_KEYINT_MAX_INFINITE;
	p.vui.i_colmatrix = 1; // bt709
	p.vui.b_fullrange = 0;
	p.b_repeat_headers = 1;
	p.b_annexb = 1;
	int kbps = 0, send = 0, cur = -1;
	if (fscanf(sf, "%d %d", &kbps, &send) != 2)
		return 1;
	set_rate(&p, kbps);
	cur = kbps;
	if (x264_param_apply_profile(&p, "main") < 0)
		return 1;
	x264_t *enc = x264_encoder_open(&p);
	if (!enc)
		return 1;
	x264_picture_t pic, out;
	if (x264_picture_alloc(&pic, X264_CSP_I420, w, h) < 0)
		return 1;
	size_t ysz = (size_t)w * h, csz = ysz / 4;
	for (long i = 0;; i++) {
		if (i > 0 && fscanf(sf, "%d %d", &kbps, &send) != 2)
			break;
		if (fread(pic.img.plane[0], 1, ysz, stdin) != ysz ||
		    fread(pic.img.plane[1], 1, csz, stdin) != csz ||
		    fread(pic.img.plane[2], 1, csz, stdin) != csz)
			break;
		if (kbps != cur) {
			set_rate(&p, kbps);
			if (x264_encoder_reconfig(enc, &p) < 0)
				fprintf(stderr, "reconfig %d failed\n", kbps);
			cur = kbps;
		}
		if (!send) {
			printf("0\n");
			continue;
		}
		pic.i_pts = i; // pts у тиках кадру: пропуски видно ратеконтролю
		x264_nal_t *nal;
		int nn;
		int sz = x264_encoder_encode(enc, &nal, &nn, &pic, &out);
		if (sz < 0)
			return 1;
		printf("%d\n", sz);
	}
	x264_picture_clean(&pic);
	x264_encoder_close(enc);
	return 0;
}
