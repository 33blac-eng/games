"""meshctrl Shell через PTY: python mshell.py <ім'я ПК> "<cmd команда>" """
import sys, time, re
sys.path.insert(0, r"F:/GITEA/OrganicOils ERP/deploy")
from _vps import vps_ssh, vps_exec
M = ("node /opt/meshcentral/node_modules/meshcentral/meshctrl.js --url ws://localhost:4430 "
     "--loginuser mykhailo --loginkeyfile /opt/meshcentral/meshcentral-data/.erp-login-key")
name, cmd = sys.argv[1], sys.argv[2]
o = vps_exec(f"{M} listdevices 2>/dev/null")[1]
nid = next(m.group(1) for m in re.finditer(r'"([^"]+)",\s*"([^"]+)"', o) if m.group(2) == name)
ssh = vps_ssh(); ch = ssh.get_transport().open_session(); ch.get_pty(width=250); ch.exec_command(f"{M} shell --id 'node//{nid}'")
def drain(quiet=12, cap=90):
    buf, last, t0 = b"", time.time(), time.time()
    while time.time() - last < quiet and time.time() - t0 < cap:
        if ch.recv_ready(): buf += ch.recv(65536); last = time.time()
        else: time.sleep(0.3)
    return buf.decode("utf-8", "replace")
print(drain(6)[-500:]); ch.send(cmd + "\r\n"); print(drain()[-3000:]); ch.close(); ssh.close()
