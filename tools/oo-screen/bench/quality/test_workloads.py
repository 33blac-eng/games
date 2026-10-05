import os, sys, unittest
import numpy as np
sys.path.insert(0, os.path.dirname(__file__))
import workloads as WL


class T(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.s = WL.Sources()

    def test_idle_static(self):
        w = WL.make("idle", self.s)
        self.assertEqual(WL.changed_flags([w.frame(i) for i in range(5)]), [True, False, False, False, False])

    def test_shapes_and_determinism(self):
        for k in WL.KINDS:
            w1, w2 = WL.make(k, self.s), WL.make(k, self.s)
            f = w1.frame(37)
            self.assertEqual(f.shape, (WL.H, WL.W, 3))
            self.assertEqual(f.dtype, np.uint8)
            np.testing.assert_array_equal(f, w2.frame(37))

    def test_scroll_shift(self):
        w = WL.make("scroll", self.s)
        d = int(w.off[1] - w.off[0])
        self.assertTrue(3 <= d <= 10)
        np.testing.assert_array_equal(w.frame(0)[d:], w.frame(1)[:-d])

    def test_typing_changes_small(self):
        w = WL.make("typing", self.s)
        a, b = w.frame(4), w.frame(5)  # glyph at 150 ms boundary
        diff = np.any(a != b, axis=2)
        self.assertTrue(0 < diff.sum() < 500)

    def test_video_only_corner(self):
        w = WL.make("video", self.s)
        diff = np.any(w.frame(0) != w.frame(1), axis=2)
        ys, xs = np.nonzero(diff)
        self.assertGreaterEqual(ys.min(), WL.H - 400)
        self.assertGreaterEqual(xs.min(), WL.W - 700)

    def test_mixed_schedule(self):
        sc = WL.mixed_schedule()
        self.assertEqual(sc[0][1], 0)
        self.assertEqual(sum(l for _, _, l in sc), WL.FPS * 60)
        tot = {}
        for k, _, l in sc:
            tot[k] = tot.get(k, 0) + l
        for k, p in WL.MIX.items():
            self.assertAlmostEqual(tot[k] / (WL.FPS * 60), p, delta=0.02)

    def test_runs_and_refine(self):
        ch = [True, False, False, True] + [False] * 9
        self.assertEqual(WL.still_runs(ch), [(0, 3), (3, 13)])
        self.assertEqual(WL.refine_frames(3, 13, 6, (2,)), [9, 11])
        self.assertEqual(WL.refine_frames(3, 10, 6, (2,)), [9])
        self.assertEqual(WL.refine_frames(0, 3, 6, (1,)), [])


if __name__ == "__main__":
    unittest.main()
