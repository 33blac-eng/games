import os, sys, unittest
sys.path.insert(0, os.path.dirname(__file__))
import lowmotion_run as L


class T(unittest.TestCase):
    def test_typing_lowers_after_hold_scroll_lifts(self):
        area = [0.005] * 30 + [0.6]
        fr = L.policy(area, [True] * len(area))
        self.assertEqual(fr[0], 1.0)
        self.assertEqual(fr[14], 1.0)  # 467 ms < hold 500 ms
        self.assertEqual(fr[15], 0.25)  # 500 ms
        self.assertEqual(fr[-1], 1.0)

    def test_video_corner_half(self):
        area = [0.11] * 90
        fr = L.policy(area, [True] * 90)
        self.assertEqual(fr[-1], 0.5)


if __name__ == "__main__":
    unittest.main()
