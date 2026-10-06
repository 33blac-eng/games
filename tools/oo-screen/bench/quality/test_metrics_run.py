"""CI checks for metrics_run.py (bench-python job: `python -m unittest discover`). Small crops only."""
import copy, glob, io, json, math, os, sys, tempfile, unittest
from contextlib import redirect_stdout
import numpy as np
from PIL import Image

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import gen_corpus as G  # noqa: E402
import metrics as M  # noqa: E402
import metrics_run as R  # noqa: E402
import pipeline as P  # noqa: E402

SCREENS = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "corpus", "screens")


class AgentConfig(unittest.TestCase):
    def test_parsed_from_tree(self):
        cfg = R.agent_config()
        # a parse miss means the agent source moved: update the regex in metrics_run.agent_config
        self.assertEqual(cfg["_defaults"], [])
        # Chrome WebRTC accepts only Constrained Baseline / Main (TASK.md: High -> hub 415)
        self.assertIn(cfg["profile"], ("Main", "Baseline", "ConstrainedBase"))
        self.assertEqual(cfg["bframes"], 0)
        self.assertGreater(cfg["peak_ratio"], 1.0)
        self.assertTrue(cfg["refine_qps"] and all(0 < q <= 51 for q in cfg["refine_qps"]))

    def test_auto_bitrate(self):
        cfg = dict(R.DEFAULT_CFG)
        self.assertEqual(R.auto_kbps(cfg, 1920, 1080), 8000)
        self.assertEqual(R.auto_kbps(cfg, 2560, 1440), 14222)
        self.assertEqual(R.auto_kbps(cfg, 3840, 2160), 30000)  # capped

    def test_arith(self):
        self.assertEqual(R._arith("mean / 2 * 3", mean=1.0), 1.5)
        self.assertEqual(R._arith("1920 * 1080"), 2073600)
        with self.assertRaises(ValueError):
            R._arith("__import__('os')")


class TextMetrics(unittest.TestCase):
    def test_contrast(self):
        img = np.full((40, 40, 3), 255, np.uint8); img[10:30, 18:21] = 0
        self.assertAlmostEqual(M.text_contrast(img, img), 1.0)
        washed = (img.astype(float) * 0.5 + 127).astype(np.uint8)
        self.assertLess(M.text_contrast(img, washed), 0.6)

    def test_masked_psnr(self):
        a = np.zeros((8, 8)); b = a.copy(); b[0, 0] = 10
        m = np.zeros((8, 8), bool); m[0, :2] = True
        self.assertAlmostEqual(M.masked_psnr(a, b, m), 10 * math.log10(255 ** 2 / 50))
        self.assertTrue(math.isnan(M.masked_psnr(a, b, np.zeros((8, 8), bool))))

    def test_cleartype_fringes_and_420_loss(self):
        im = Image.new("RGB", (120, 24), "white")
        G.subpixel_text(im, (2, 4), "Illegal 1|l", G.UI, 11, (0, 0, 0))
        a = np.asarray(im).astype(int)
        self.assertGreater(np.abs(a[..., 0] - a[..., 2]).max(), 40)  # coloured stem edges
        mask = M.text_mask(a.astype(np.uint8))
        _, cb0, cr0 = P.rgb_to_yuv(a.astype(np.uint8))
        out, _ = P.run(a.astype(np.uint8), "420")
        _, cb1, cr1 = P.rgb_to_yuv(out)
        self.assertLess(M.masked_psnr(cb0, cb1, mask), 35)  # 4:2:0 smears the fringes


class Corpus(unittest.TestCase):
    def test_screens_present_and_small(self):
        paths = sorted(glob.glob(os.path.join(SCREENS, "*.png")))
        names = {os.path.basename(p) for p in paths}
        for n in ("excel-1080p.png", "tinyfont-1080p.png", "cleartype-1080p.png", "desktop-1080p.png"):
            self.assertIn(n, names)
        self.assertLess(sum(os.path.getsize(p) for p in paths), 5 * 1024 * 1024)


def _crop_png(td):
    src = os.path.join(SCREENS, "excel-1080p.png")
    p = os.path.join(td, "excel-crop.png")
    Image.open(src).convert("RGB").crop((0, 140, 160, 236)).save(p)
    return p


@unittest.skipUnless(P.have_ffmpeg(), "ffmpeg not installed")
class EndToEnd(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.td = tempfile.TemporaryDirectory()
        cls.png = _crop_png(cls.td.name)
        cls.variants = ["raw420", "main-idr", "main-steady", "cbase-idr", "main-refine"]
        cls.rows = R.run_image(cls.png, cls.variants, R.agent_config(), frames=3, log=lambda *_: None)

    @classmethod
    def tearDownClass(cls):
        cls.td.cleanup()

    def test_rows(self):
        self.assertEqual([r["variant"] for r in self.rows], self.variants)
        by = {r["variant"]: r for r in self.rows}
        self.assertTrue(by["main-idr"]["plid"].startswith("4d"))
        self.assertTrue(by["cbase-idr"]["plid"].startswith("42"))
        self.assertTrue(by["main-refine"]["plid"].startswith("4d"))
        self.assertIsNone(by["raw420"]["metrics"]["kbit"])
        for r in self.rows:
            m = r["metrics"]
            self.assertEqual(set(m), set(R.METRICS))
            self.assertLessEqual(m["ssim"], 1.0 + 1e-9)
            self.assertGreater(m["psnr_y"], 20)
        # the codec can only lose information relative to the 4:2:0 colour chain alone
        self.assertGreaterEqual(by["raw420"]["metrics"]["psnr_y"], by["main-idr"]["metrics"]["psnr_y"])

    def test_deterministic(self):
        again = R.run_image(self.png, ["main-steady"], R.agent_config(), frames=3, log=lambda *_: None)
        self.assertEqual(again[0]["metrics"], [r for r in self.rows if r["variant"] == "main-steady"][0]["metrics"])

    def test_cli_run_and_compare(self):
        td = self.td.name
        a_json, a_md = os.path.join(td, "a.json"), os.path.join(td, "a.md")
        with redirect_stdout(io.StringIO()):
            R.main(["run", "--corpus", td, "--variants", "raw420,main-steady", "--frames", "2",
                    "--no-vmaf", "--json", a_json, "--md", a_md])
        md = open(a_md).read()
        self.assertIn("| variant | n | PSNR dB", md)
        self.assertIn("### excel-crop.png", md)
        doc = json.load(open(a_json))
        self.assertEqual(doc["schema"], R.SCHEMA)
        self.assertEqual(len(doc["rows"]), 2)
        # identical runs: no regressions
        _, regs = R.compare(doc, doc)
        self.assertEqual(regs, [])
        # degrade one row: flagged, and --fail-on-regression exits 1
        worse = copy.deepcopy(doc)
        worse["rows"][1]["metrics"]["txt_contrast"] = worse["rows"][1]["metrics"]["txt_contrast"] - 0.2
        worse["rows"][1]["metrics"]["psnr"] -= 1.0
        worse["agent_config"]["refs"] = 1
        md2, regs = R.compare(doc, worse)
        self.assertEqual({r["metric"] for r in regs}, {"txt_contrast", "psnr"})
        self.assertIn("`refs`: 2 -> 1", md2)
        b_json = os.path.join(td, "b.json")
        with open(b_json, "w") as f:
            f.write(R.dump_json(worse))
        with redirect_stdout(io.StringIO()):
            R.main(["compare", a_json, b_json])
            with self.assertRaises(SystemExit) as cm:
                R.main(["compare", a_json, b_json, "--fail-on-regression"])
        self.assertEqual(cm.exception.code, 1)


if __name__ == "__main__":
    unittest.main()
