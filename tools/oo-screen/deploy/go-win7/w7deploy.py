"""Розкочування oo-agent на Windows 7 (без PowerShell ScheduledTasks і без
мертвого meshctrl RunCommand): exe + XML задачі файловим тунелем Mesh, далі
один .cmd через термінал агента (mshell). python w7deploy.py <ПК> <user>"""
import os, re, subprocess, sys, time
sys.path.insert(0, r"F:/GITEA/OrganicOils ERP/deploy")
from _vps import vps_exec

PC, USER = sys.argv[1], sys.argv[2]
HERE = os.path.dirname(os.path.abspath(__file__))
EXE = os.path.join(HERE, "oo-agent-w7.exe")
DIR = r"C:\ProgramData\OrganicOils\ScreenAgent"
M = ("node /opt/meshcentral/node_modules/meshcentral/meshctrl.js --url ws://localhost:4430 "
     "--loginuser mykhailo --loginkeyfile /opt/meshcentral/meshcentral-data/.erp-login-key")
nid = next(m.group(1) for m in re.finditer(r'"([^"]+)",\s*"([^"]+)"', vps_exec(f"{M} listdevices 2>/dev/null")[1]) if m.group(2) == PC)
tok = vps_exec("grep ^OO_SCREEN_T1_TOKEN= /etc/oo-screen/hub.env | cut -d= -f2-")[1].strip()
assert tok
args = (f"-transport webrtc -hub https://remote.organicoils.com.ua/offer/agent -node node//{nid} "
        f"-token {tok} -log {DIR}\\logs\\oo-agent.log -audio -input")
xml = f"""<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <Triggers>
    <LogonTrigger><Enabled>true</Enabled><UserId>{USER}</UserId></LogonTrigger>
    <TimeTrigger><Repetition><Interval>PT5M</Interval><StopAtDurationEnd>false</StopAtDurationEnd></Repetition>
      <StartBoundary>2026-01-01T00:00:00</StartBoundary><Enabled>true</Enabled></TimeTrigger>
  </Triggers>
  <Principals><Principal id="Author"><UserId>{USER}</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit><Enabled>true</Enabled><Hidden>false</Hidden>
    <StartWhenAvailable>true</StartWhenAvailable>
  </Settings>
  <Actions Context="Author"><Exec><Command>{DIR}\\oo-agent.exe</Command><Arguments>{args}</Arguments></Exec></Actions>
</Task>
"""
xp = os.path.join(HERE, f"_task_{PC}.xml")
with open(xp, "w", encoding="utf-16") as f:
    f.write(xml)
cmd = os.path.join(HERE, "_w7install.cmd")
with open(cmd, "w", newline="\r\n") as f:
    f.write(f"""@echo off
cd /d {DIR}
schtasks /end /tn oo-screen-pilot 1>NUL 2>&1
taskkill /f /im oo-agent.exe 1>NUL 2>&1
taskkill /f /im oo-agent-w7c.exe 1>NUL 2>&1
if exist oo-agent.exe move /y oo-agent.exe oo-agent.exe.bak-w7 1>NUL
move /y oo-agent.exe.new oo-agent.exe 1>NUL
del /q hello.exe _mup_hello.exe h.txt a.txt b.txt probe.cmd probe2.cmd oo-agent-w7c.exe 2>NUL
if not exist logs mkdir logs
icacls "{DIR}" /inheritance:r /grant *S-1-5-18:(OI)(CI)F *S-1-5-32-544:(OI)(CI)F *S-1-5-32-545:(OI)(CI)RX 1>NUL
icacls "{DIR}\\logs" /grant *S-1-5-32-545:(OI)(CI)M 1>NUL
schtasks /create /tn oo-screen-pilot /xml "{DIR}\\_task.xml" /f
del /q "{DIR}\\_task.xml"
schtasks /run /tn oo-screen-pilot
echo OO-W7-DONE
""")


def up(local, name):
    r = subprocess.run([sys.executable, os.path.join(HERE, "mup.py"), PC, local, DIR],
                       capture_output=True, text=True, env=dict(os.environ, MSYS_NO_PATHCONV="1"))
    assert "Upload done" in r.stdout, r.stdout + r.stderr
    return name


try:
    # mup.py кладе файл під іменем _mup/<basename>; перейменовуємо в .cmd нижче.
    for local, final in ((EXE, "oo-agent.exe.new"), (xp, "_task.xml"), (cmd, "_w7install.cmd")):
        tmp = os.path.join(HERE, final)
        if os.path.abspath(local) != os.path.abspath(tmp):
            with open(local, "rb") as a, open(tmp, "wb") as b:
                b.write(a.read())
        up(tmp, final)
        if tmp != local:
            os.remove(tmp)
finally:
    os.remove(xp)
since = time.strftime("%Y-%m-%d %H:%M:%S", time.gmtime(time.time() - 5))
out = subprocess.run([sys.executable, os.path.join(HERE, "mshell.py"), PC, DIR + r"\_w7install.cmd"],
                     capture_output=True, text=True, encoding="utf-8", errors="replace",
                     env=dict(os.environ, MSYS_NO_PATHCONV="1")).stdout
out = re.sub(r"\x1b\[[0-9;]*[A-Za-z]", " ", out).replace(tok, "<token>")
print("install:", "OO-W7-DONE" in out, re.sub(r"\s+", " ", out)[-600:])
for _ in range(12):
    time.sleep(10)
    j = vps_exec(f"journalctl -u oo-screen-hub-webrtc-t1 --since '{since} UTC' --no-pager | grep -F 'node//{nid}' | grep -E 'publisher up|кодек|agent leg ICE' | tail -3")[1]
    if "publisher up" in j:
        break
print("hub:", j.strip()[-500:] or "нема ноги агента")
