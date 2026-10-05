import sys, numpy as np
from PIL import Image, ImageDraw, ImageFont
sys.path.insert(0, '.')
import pipeline as P, final as F
font = ImageFont.truetype('/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf', 22)
rows = []
for name, box in [('colortext-1440p', (40, 40, 520, 260)), ('sheet-1440p', (0, 0, 480, 220)), ('code-dark-1440p', (0, 30, 480, 250))]:
    path = f'../corpus/{name}.png'
    ref = np.asarray(Image.open(path).convert('RGB'))
    before, _ = P.run(ref, '420', 1080 / ref.shape[0], 'bicubic', 8000)
    after, _ = P.run(ref, '420', None, 'bicubic', qp=18, frames=1)
    after = P.paste_tiles(after.copy(), ref, F.select_tiles(path))
    x0, y0, x1, y1 = box
    crops = [np.asarray(a)[y0:y1, x0:x1] for a in (before, after, ref)]
    crops = [Image.fromarray(np.clip(c, 0, 255).astype('uint8')).resize(((x1-x0)*2, (y1-y0)*2), Image.NEAREST) for c in crops]
    rows.append(crops)
cw, ch = rows[0][0].size
pad, head = 16, 44
out = Image.new('RGB', (3 * cw + 4 * pad, len(rows) * (ch + pad) + head + pad), (245, 245, 245))
d = ImageDraw.Draw(out)
for i, t in enumerate(['БУЛО (1080p + 4:2:0)', 'СТАЛО (рідна + refine + тайли)', 'ОРИГІНАЛ']):
    d.text((pad + i * (cw + pad), 10), t, fill=(180, 30, 30) if i == 0 else (20, 120, 40) if i == 1 else (40, 40, 40), font=font)
for r, crops in enumerate(rows):
    for i, c in enumerate(crops):
        out.paste(c, (pad + i * (cw + pad), head + r * (ch + pad)))
out.save('/home/user/games/before-after.png')
print(out.size)
