import os, sys, unittest
import numpy as np
sys.path.insert(0, os.path.dirname(__file__))
import avc444 as A


class TestAVC444Split(unittest.TestCase):
    def planes(self, h, w, seed=0):
        r = np.random.default_rng(seed)
        return tuple(r.integers(0, 256, (h, w)).astype(np.uint8) for _ in range(3))

    def test_roundtrip_lossless(self):
        for h, w in ((16, 16), (32, 48), (1088, 1920)):
            y, u, v = self.planes(h, w)
            main, aux = A.split444(y, u, v)
            for p, shp in zip(main + aux, [(h, w), (h // 2, w // 2), (h // 2, w // 2)] * 2):
                self.assertEqual(p.shape, shp)
            for a, b in zip((y, u, v), A.combine444(main, aux)):
                np.testing.assert_array_equal(a, b)

    def test_aux_layout(self):
        y, u, v = self.planes(32, 16, 1)
        _, (ay, au, av) = A.split444(y, u, v)
        np.testing.assert_array_equal(ay[0:8], u[1:16:2])
        np.testing.assert_array_equal(ay[8:16], v[1:16:2])
        np.testing.assert_array_equal(ay[16:24], u[17:32:2])
        np.testing.assert_array_equal(ay[24:32], v[17:32:2])
        np.testing.assert_array_equal(au, u[0::2, 1::2])
        np.testing.assert_array_equal(av, v[0::2, 1::2])

    def test_pad16_and_rgb_roundtrip(self):
        rgb = np.random.default_rng(2).integers(0, 256, (33, 47, 3)).astype(np.uint8)
        y, u, v = A.yuv444(rgb)
        self.assertEqual(y.shape, (48, 48))
        out = A.to_rgb(*A.combine444(*A.split444(y, u, v)), 33, 47)
        self.assertEqual(out.shape, rgb.shape)
        ref = A.to_rgb(y, u, v, 33, 47)  # no-split path: identical result
        np.testing.assert_array_equal(out, ref)

    def test_text_tiles(self):
        m = np.zeros((20, 20), bool); m[17, 3] = True
        self.assertEqual(A.text_tiles(m, 32, 32).tolist(), [[False, False], [True, False]])


if __name__ == "__main__":
    unittest.main()
