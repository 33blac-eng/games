import math, os, sys, unittest
import numpy as np
sys.path.insert(0, os.path.dirname(__file__))
import metrics as M


class T(unittest.TestCase):
    def setUp(self):
        r = np.random.default_rng(1)
        self.img = r.integers(0, 256, (64, 80, 3)).astype(np.uint8)

    def test_identical(self):
        self.assertEqual(M.psnr(self.img, self.img), math.inf)
        self.assertAlmostEqual(M.ssim(self.img, self.img), 1.0, places=9)
        es, er = M.text_sharpness(self.img, self.img)
        self.assertAlmostEqual(es, 1.0, places=9)
        self.assertAlmostEqual(er, 1.0, places=9)

    def test_known_noise(self):
        # constant +/-10 error -> MSE 100 -> PSNR = 10*log10(255^2/100) = 28.1308
        a = np.full((32, 32), 128.0)
        b = a + np.where(np.indices(a.shape).sum(0) % 2 == 0, 10.0, -10.0)
        self.assertAlmostEqual(M.psnr(a, b), 10 * math.log10(255 ** 2 / 100), places=6)
        # gaussian noise sigma 20 lowers SSIM well below 1 but above 0
        r = np.random.default_rng(2)
        n = np.clip(self.img + r.normal(0, 20, self.img.shape), 0, 255)
        s = M.ssim(self.img, n)
        self.assertTrue(0.5 < s < 0.99, s)

    def test_blur_lowers_sharpness(self):
        img = np.zeros((40, 40)); img[:, 20:] = 255
        blur = img.copy(); blur[:, 19:22] = [64, 128, 192]
        es, er = M.text_sharpness(np.stack([img] * 3, -1), np.stack([blur] * 3, -1))
        self.assertLess(er, 1.0)



class Pipe(unittest.TestCase):
    def test_444_roundtrip_near_lossless(self):
        import pipeline as P
        r = np.random.default_rng(3)
        img = r.integers(16, 236, (32, 32, 3)).astype(np.uint8)
        out, _ = P.run(img, "444")
        self.assertGreater(M.psnr(img, out), 40)

    def test_420_hurts_red_on_white(self):
        import pipeline as P
        img = np.full((32, 32, 3), 255, np.uint8); img[:, 15] = (220, 0, 0)
        o444, _ = P.run(img, "444"); o420, _ = P.run(img, "420")
        self.assertLess(M.psnr(img, o420), M.psnr(img, o444))


if __name__ == "__main__":
    unittest.main()
