import os, sys, unittest
import numpy as np
sys.path.insert(0, os.path.dirname(__file__))
import videomode_run as V
import workloads as WL


class T(unittest.TestCase):
    def test_enter_after_hold_and_exit(self):
        cm = V.ContentMode()
        modes = [cm.update(0.11, 0, j / 30) for j in range(1, 40)]
        self.assertNotEqual(modes[28], "video")  # < 1 s
        self.assertEqual(modes[-1], "video")
        # 5 % holds, < 4 % lets it go after 1.5 s
        t = 40 / 30
        for j in range(60):
            t += 1 / 30
            self.assertEqual(cm.update(0.05, 0, t), "video")
        for j in range(44):
            t += 1 / 30
            m = cm.update(0.01, 0, t)
        self.assertEqual(m, "video")
        for j in range(3):
            t += 1 / 30
            m = cm.update(0.01, 0, t)
        self.assertNotEqual(m, "video")

    def test_small_motion_never_video(self):
        cm = V.ContentMode()
        self.assertFalse(any(cm.update(0.09, 0, j / 30) == "video" for j in range(300)))

    def test_no_video(self):
        cm = V.ContentMode(no_video=True)
        self.assertEqual([cm.update(1, 0, j / 30) for j in range(100)][-1], "normal")

    def test_admit_rates(self):
        fr = [0.5] * 600  # 10 s of 60 Hz motion
        a, m = V.admit(fr, "normal")
        self.assertAlmostEqual(sum(a), 300, delta=2)
        a, m = V.admit(fr, "video")
        self.assertAlmostEqual(sum(a), 30 + 540, delta=3)  # 1 s at 30, then 60
        a, _ = V.admit(fr, "cap15")
        self.assertAlmostEqual(sum(a), 150, delta=2)

    def test_held_frame_flushed(self):
        fr = [0.5, 0.5, 0, 0, 0]
        a, _ = V.admit(fr, "normal")
        self.assertEqual(a, [True, False, True, False, False])

    def test_ticker_interpolates(self):
        s = WL.Sources()
        t = V.Ticker("scroll", s)
        w = WL.make("scroll", s)
        np.testing.assert_array_equal(t.frame(4), w.frame(2))
        self.assertFalse(np.array_equal(t.frame(3), t.frame(2)))
        v = V.Ticker("video", s)
        self.assertEqual(v.frame(1).shape, (WL.H, WL.W, 3))


if __name__ == "__main__":
    unittest.main()
