// x264rc — ОДИН безперервний енкод x264, яким покадрово керує симуляція
// політик rate control агента (TASK.md крок 4, bench/quality/ratecontrol_run.py):
// примусовий IDR, примусовий QP (refine), межі QP, інтра-рефреш, ROI (quant
// offsets), пропуск незмінених кадрів. Це СИМУЛЯЦІЯ: x264, а не Media
// Foundation MFT агента.
//
// argv: W H FPS out.h264 dump.yuv [key=val ...]
//   key: qpmin, qpmax (межі QP, як CODECAPI_AVEncVideoMinQP/MaxQP),
//        keyint (періодичний IDR у ЗАКОДОВАНИХ кадрах, 0 = безкінечний),
//        ir=1 (periodic intra refresh замість IDR), aq (x264 aq-mode).
// stdin — запит/відповідь (викликач чекає відповідь перед наступним кадром,
// бо політика залежить від розміру й QP попереднього):
//   рядок "<kbps> <newpix> <send> <idr> <qp> <dump> [n x0 y0 x1 y1 delta ...]\n",
//   далі, якщо newpix=1, — сирий кадр yuv420p W×H (інакше пікселі попереднього).
//   send=0 — кадр не кодується (агент його не подає); idr=1 — примусовий IDR;
//   qp>0 — примусовий QP кадру; dump=1 — дописати ПОКАЗАНИЙ декодером кадр
//   (реконструкцію останнього закодованого) у dump.yuv; далі — ROI: n
//   прямокутників у пікселях із дельтою QP (від'ємна = якісніше).
// stdout: по рядку на запит: "<bytes> <type> <qp>", type I/P/- (не
//   кодувався), qp — QP кадру за x264.
//
// Збірка: gcc -O2 -o x264rc x264rc.c -lx264   (потрібен libx264-dev)
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
	if (argc < 6) {
		fprintf(stderr, "usage: x264rc W H FPS out.h264 dump.yuv [key=val ...]\n");
		return 2;
	}
	int w = atoi(argv[1]), h = atoi(argv[2]), fps = atoi(argv[3]);
	FILE *of = fopen(argv[4], "wb");
	FILE *df = fopen(argv[5], "wb");
	if (!of || !df) {
		perror("open");
		return 1;
	}
	int qpmin = -1, qpmax = -1, keyint = 0, ir = 0, aq = -1;
	for (int i = 6; i < argc; i++) {
		char k[32];
		int v;
		if (sscanf(argv[i], "%31[^=]=%d", k, &v) != 2)
			continue;
		if (!strcmp(k, "qpmin"))
			qpmin = v;
		else if (!strcmp(k, "qpmax"))
			qpmax = v;
		else if (!strcmp(k, "keyint"))
			keyint = v;
		else if (!strcmp(k, "ir"))
			ir = v;
		else if (!strcmp(k, "aq"))
			aq = v;
	}
	char line[16384];
	int kbps = 0, newpix = 0, send = 0, idr = 0, qp = 0, dump = 0, off = 0, cur = -1;
	if (!fgets(line, sizeof line, stdin) ||
	    sscanf(line, "%d %d %d %d %d %d%n", &kbps, &newpix, &send, &idr, &qp, &dump, &off) < 6)
		return 1;
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
	p.i_keyint_max = keyint > 0 ? keyint : X264_KEYINT_MAX_INFINITE;
	p.i_keyint_min = 1;
	p.i_scenecut_threshold = 0; // MFT не вставляє IDR на зміні сцени
	p.b_intra_refresh = ir;
	p.b_full_recon = 1;
	p.vui.i_colmatrix = 1; // bt709
	p.vui.b_fullrange = 0;
	p.b_repeat_headers = 1;
	p.b_annexb = 1;
	p.i_log_level = X264_LOG_WARNING;
	if (qpmin >= 0)
		p.rc.i_qp_min = qpmin;
	if (qpmax >= 0)
		p.rc.i_qp_max = qpmax;
	if (aq >= 0)
		p.rc.i_aq_mode = aq;
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
	uint8_t *shown = calloc(1, ysz + 2 * csz); // що зараз показує декодер
	int mbw = (w + 15) / 16, mbh = (h + 15) / 16;
	float *qo = calloc((size_t)mbw * mbh, sizeof(float));
	for (long i = 0;; i++) {
		if (i > 0) {
			if (!fgets(line, sizeof line, stdin))
				break;
			if (sscanf(line, "%d %d %d %d %d %d%n", &kbps, &newpix, &send, &idr, &qp, &dump, &off) < 6)
				break;
		}
		if (newpix && (fread(pic.img.plane[0], 1, ysz, stdin) != ysz ||
		               fread(pic.img.plane[1], 1, csz, stdin) != csz ||
		               fread(pic.img.plane[2], 1, csz, stdin) != csz))
			break;
		if (kbps != cur) {
			set_rate(&p, kbps);
			if (x264_encoder_reconfig(enc, &p) < 0)
				fprintf(stderr, "reconfig %d failed\n", kbps);
			cur = kbps;
		}
		if (!send) {
			printf("0 - 0\n");
		} else {
			int n = 0, roi = 0, adv = 0;
			const char *s = line + off;
			if (sscanf(s, "%d%n", &n, &adv) == 1 && n > 0) {
				memset(qo, 0, (size_t)mbw * mbh * sizeof(float));
				s += adv;
				for (int r = 0; r < n; r++) {
					int x0, y0, x1, y1;
					float d;
					if (sscanf(s, "%d %d %d %d %f%n", &x0, &y0, &x1, &y1, &d, &adv) != 5)
						break;
					s += adv;
					for (int my = y0 / 16; my < mbh && my * 16 < y1; my++)
						for (int mx = x0 / 16; mx < mbw && mx * 16 < x1; mx++)
							qo[my * mbw + mx] = d;
				}
				roi = 1;
			}
			pic.i_pts = i; // pts у тиках кадру: пропуски видно ратеконтролю
			pic.i_type = idr ? X264_TYPE_IDR : X264_TYPE_AUTO;
			pic.i_qpplus1 = qp > 0 ? qp + 1 : X264_QP_AUTO;
			pic.prop.quant_offsets = roi ? qo : NULL;
			pic.prop.quant_offsets_free = NULL;
			x264_nal_t *nal;
			int nn;
			int sz = x264_encoder_encode(enc, &nal, &nn, &pic, &out);
			if (sz < 0)
				return 1;
			if (sz > 0)
				fwrite(nal[0].p_payload, 1, (size_t)sz, of);
			// zerolatency: без затримки, out — реконструкція саме цього кадру.
			for (int y = 0; y < h; y++)
				memcpy(shown + (size_t)y * w, out.img.plane[0] + (size_t)y * out.img.i_stride[0], (size_t)w);
			if ((out.img.i_csp & X264_CSP_MASK) == X264_CSP_NV12) {
				// Внутрішня реконструкція x264 — NV12: розплітаємо UV.
				for (int y = 0; y < h / 2; y++) {
					const uint8_t *uv = out.img.plane[1] + (size_t)y * out.img.i_stride[1];
					uint8_t *u = shown + ysz + (size_t)y * (w / 2), *v = u + csz;
					for (int x = 0; x < w / 2; x++) {
						u[x] = uv[2 * x];
						v[x] = uv[2 * x + 1];
					}
				}
			} else {
				for (int c = 1; c <= 2; c++)
					for (int y = 0; y < h / 2; y++)
						memcpy(shown + ysz + (size_t)(c - 1) * csz + (size_t)y * (w / 2),
						       out.img.plane[c] + (size_t)y * out.img.i_stride[c], (size_t)w / 2);
			}
			printf("%d %c %d\n", sz, IS_X264_TYPE_I(out.i_type) ? 'I' : 'P', out.i_qpplus1 - 1);
		}
		if (dump)
			fwrite(shown, 1, ysz + 2 * csz, df);
		fflush(stdout);
	}
	fclose(of);
	fclose(df);
	x264_picture_clean(&pic);
	x264_encoder_close(enc);
	return 0;
}
