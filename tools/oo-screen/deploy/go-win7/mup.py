"""python mup.py <ПК> <локальний файл> <тека на ПК> — upload через meshctrl"""
import sys, re, os
sys.path.insert(0, r"F:/GITEA/OrganicOils ERP/deploy")
from _vps import vps_sftp, vps_exec
M = ("node /opt/meshcentral/node_modules/meshcentral/meshctrl.js --url ws://localhost:4430 "
     "--loginuser mykhailo --loginkeyfile /opt/meshcentral/meshcentral-data/.erp-login-key")
name, local, target = sys.argv[1:4]
o = vps_exec(f"{M} listdevices 2>/dev/null")[1]
nid = next(m.group(1) for m in re.finditer(r'"([^"]+)",\s*"([^"]+)"', o) if m.group(2) == name)
os.makedirs("x",exist_ok=True); rp = "/root/_mup/" + os.path.basename(local); vps_exec("mkdir -p /root/_mup")
ssh, s = vps_sftp(); s.put(local, rp); s.close(); ssh.close()
c, out, err = vps_exec(f"{M} upload --id 'node//{nid}' --file {rp} --target '{target}' 2>&1; rm -f {rp}", timeout=600)
print(out[-800:], err[-300:])
