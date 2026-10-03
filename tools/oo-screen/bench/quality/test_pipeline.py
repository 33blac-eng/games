import os, sys, unittest
import numpy as np
sys.path.insert(0, os.path.dirname(__file__))
import pipeline as P


class TestSub420(unittest.TestCase):
    def test_even(self):
        c = np.arange(16, dtype=np.float64).reshape(4, 4)
        np.testing.assert_allclose(P.sub420(c), [[2.5, 4.5], [10.5, 12.5]])

    def test_odd_dims_edge_replicated(self):
        c = np.arange(15, dtype=np.float64).reshape(3, 5)
        out = P.sub420(c)
        self.assertEqual(out.shape, (2, 3))
        # last column pairs col 4 with itself, last row pairs row 2 with itself
        self.assertAlmostEqual(out[0, 2], (4 + 4 + 9 + 9) / 4)
        self.assertAlmostEqual(out[1, 0], (10 + 11 + 10 + 11) / 4)
        self.assertAlmostEqual(out[1, 2], 14.0)

    def test_run_odd_size_no_encode(self):
        rgb = np.random.default_rng(0).integers(0, 256, (33, 47, 3)).astype(np.uint8)
        out, bits = P.run(rgb, chroma="420")
        self.assertEqual(out.shape, rgb.shape)
        self.assertIsNone(bits)


if __name__ == "__main__":
    unittest.main()
