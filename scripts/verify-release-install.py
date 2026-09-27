#!/usr/bin/env python3
"""端到端证明：签名发行链路与 install.sh 只在验签通过后安装（H6）。

全部动作都在临时目录内完成，不访问网络、不写用户目录：
  keygen -> 签署 dist -> 打包 -> 解包 -> 归档验签（含平台检查）
  -> 由网关 /download/release 端点提供已签名文件
  -> install.sh 必须装出与发行目录字节一致的二进制
  -> 篡改二进制、更换公钥、缺失签名、--skip-verify、symlink 目标都必须失败

用法（在仓库根目录执行）：
    python3 scripts/verify-release-install.py                       # 自行 go build 验证器
    MESH_VERIFY_BIN=/tmp/mesh python3 scripts/verify-release-install.py  # 复用已构建验证器

退出码 0 表示全部检查通过；任一检查失败即返回 1 并打印汇总。
"""
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
VERSION = 'installer-proof'
SEQUENCE = '200'
results = {}
failures = []


def record(name, ok, detail=''):
    results[name] = bool(ok)
    print(f"[{'PASS' if ok else 'FAIL'}] {name} {detail}")
    if not ok:
        failures.append(name)


def clean_env(tools, extra=None):
    env = {k: v for k, v in os.environ.items()
           if not k.startswith(('MESH_', 'AGENT_GATEWAY_', 'GATEWAY_'))}
    if tools:
        env['PATH'] = f"{tools}:{env.get('PATH', '')}"
    if extra:
        env.update(extra)
    return env


def main():
    with tempfile.TemporaryDirectory(prefix='gateway-proof-') as td:
        tmp = Path(td)
        tools = None
        verifier = os.environ.get('MESH_VERIFY_BIN', '')
        if not verifier:
            tools = tmp / 'tools'
            tools.mkdir()
            verifier = tools / 'mesh'
            build = subprocess.run(['go', 'build', '-o', str(verifier), './cmd/mesh'],
                                   cwd=ROOT, capture_output=True, text=True)
            if build.returncode != 0:
                print(build.stdout + build.stderr)
                return 1
        verifier = Path(verifier)
        env = clean_env(tools)

        keys, data = tmp / 'keys', tmp / 'gateway'
        (tmp / 'other-keys').mkdir()
        keys.mkdir()
        dist = data / 'dist'
        dist.mkdir(parents=True)
        shutil.copy2(verifier, dist / 'mesh-darwin-arm64')
        for name in ('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES', 'README.md'):
            shutil.copy2(ROOT / name, dist / name)

        def run(args, **kw):
            return subprocess.run([str(verifier), *args], cwd=tmp, env=env,
                                  capture_output=True, text=True, timeout=120, **kw)

        r = run(['release', 'keygen', '--dir', str(keys)])
        record('keygen', r.returncode == 0, r.stderr.strip()[:120])
        if not (keys / 'release.pub').is_file() or not (keys / 'release.key').is_file():
            print('keygen precondition failed; aborting')
            return 1

        r = run(['release', 'sign', '--dir', str(dist), '--key', str(keys / 'absent.key'),
                 '--version', VERSION, '--sequence', SEQUENCE])
        record('sign_without_key_fails', r.returncode != 0, r.stderr.strip()[:120])
        r = run(['release', 'sign', '--dir', str(dist), '--key', str(keys / 'release.key'),
                 '--version', VERSION, '--sequence', SEQUENCE])
        record('sign_with_key_succeeds', r.returncode == 0,
               r.stderr.strip()[:160] or 'manifest.json + manifest.sig written')
        r = run(['release', 'verify', '--dir', str(dist), '--public-key', str(keys / 'release.pub')])
        record('verify_all_files_without_target', r.returncode == 0, r.stderr.strip()[:120])

        # 未签名目录与错误公钥都必须被拒绝。
        unsigned = tmp / 'unsigned'
        unsigned.mkdir()
        for p in dist.iterdir():
            if p.name not in ('manifest.json', 'manifest.sig'):
                shutil.copy2(p, unsigned / p.name)
        r = run(['release', 'verify', '--dir', str(unsigned), '--public-key', str(keys / 'release.pub')])
        record('unsigned_dir_rejected', r.returncode != 0, r.stderr.strip()[:120])
        run(['release', 'keygen', '--dir', str(tmp / 'other-keys')])
        r = run(['release', 'verify', '--dir', str(dist), '--public-key', str(tmp / 'other-keys' / 'release.pub')])
        record('wrong_public_key_rejected', r.returncode != 0, r.stderr.strip()[:120])

        # 归档打包与解包后验签。
        pkg = subprocess.run(['python3', str(ROOT / 'scripts/package-release.py'), str(dist), VERSION],
                             cwd=tmp, env=env, capture_output=True, text=True)
        record('package_signed_release', pkg.returncode == 0, pkg.stderr.strip()[:160])
        unpack = tmp / 'unpack'
        unpack.mkdir()
        with tarfile.open(dist / f'agent-gateway_{VERSION}_darwin-arm64.tar.gz') as archive:
            archive.extractall(unpack, filter='data')
        r = run(['release', 'verify', '--dir', str(unpack), '--public-key', str(keys / 'release.pub'),
                 '--target', 'darwin-arm64'])
        record('signed_archive_verification', r.returncode == 0, r.stderr.strip()[:160])
        r = run(['release', 'verify', '--dir', str(dist), '--public-key', str(keys / 'release.pub'),
                 '--target', 'linux-amd64'])
        record('absent_target_rejected', r.returncode != 0, r.stderr.strip()[:120])

        # 由网关提供已签名发行目录，走 install.sh 真实安装路径。
        with open(tmp / 'server.log', 'w+b') as log:
            proc = subprocess.Popen([str(verifier), 'server', '--addr', '127.0.0.1:0', '--data-dir', str(data),
                                     '--invitations', '1'], cwd=tmp, env=env, stdout=log, stderr=log)
            try:
                origin, out = None, ''
                for _ in range(400):
                    log.seek(0)
                    out = log.read().decode(errors='replace')
                    match = re.search(r'listening \(TLS\):\s*(\S+)', out)
                    if match:
                        origin = match.group(1)
                        break
                    if proc.poll() is not None:
                        raise RuntimeError('gateway exited early:\n' + out)
                    time.sleep(0.025)
                if not origin:
                    raise RuntimeError('gateway did not report a TLS address')

                secrets = [data / 'admin.password', data / 'admin.token']
                values = [p.read_text().strip() for p in secrets if p.is_file()]
                record('startup_banner_has_no_credentials',
                       all(v and v not in out for v in values) and 'password:' not in out,
                       'banner only carries file paths')
                record('installer_rejects_skip_verify_flag',
                       subprocess.run(['sh', str(ROOT / 'install.sh'), '--dir', str(tmp / 'bin-noskip'),
                                       '--skip-verify'], cwd=tmp, env=env,
                                      capture_output=True, text=True).returncode != 0,
                       'unsafe flag refused')

                def installer(*args):
                    return subprocess.run(['sh', str(ROOT / 'install.sh'), *args], cwd=tmp,
                                          env=clean_env(tools, {'CURL_CA_BUNDLE': str(data / 'ca.crt')}),
                                          capture_output=True, text=True, timeout=180)

                base = origin + '/download/release'
                common = ['--base-url', base, '--verifier', str(verifier),
                          '--public-key', str(keys / 'release.pub')]

                bindir = tmp / 'bin'
                r = installer('--dir', str(bindir), *common)
                installed = bindir / 'mesh'
                record('installer_installs_verified_binary',
                       r.returncode == 0 and installed.is_file(), (r.stdout + r.stderr).strip()[:200])
                if installed.is_file():
                    v = subprocess.run([str(installed), '--version'], capture_output=True, text=True)
                    record('installed_binary_runs', v.returncode == 0, (v.stdout + v.stderr).strip()[:120])
                    record('notices_installed',
                           all((bindir / 'agent-gateway-notices' / n).is_file()
                               for n in ('LICENSE', 'NOTICE', 'THIRD_PARTY_NOTICES')))
                    record('installed_digest_matches_release',
                           installed.read_bytes() == (dist / 'mesh-darwin-arm64').read_bytes())

                r = installer('--dir', str(tmp / 'bin-nokey'), '--base-url', base)
                record('installer_requires_verifier_and_key', r.returncode != 0,
                       (r.stdout + r.stderr).strip()[:160])
                r = installer('--dir', str(tmp / 'bin-http'), '--base-url', 'http://example.test/release',
                              '--verifier', str(verifier), '--public-key', str(keys / 'release.pub'))
                record('installer_requires_https', r.returncode != 0, (r.stdout + r.stderr).strip()[:160])

                original = (dist / 'mesh-darwin-arm64').read_bytes()
                (dist / 'mesh-darwin-arm64').write_bytes(original + b'\0tampered')
                r = installer('--dir', str(tmp / 'bin-tampered'), *common)
                record('tampered_binary_rejected', r.returncode != 0, (r.stdout + r.stderr).strip()[:200])
                (dist / 'mesh-darwin-arm64').write_bytes(original)

                sig = (dist / 'manifest.sig').read_bytes()
                (dist / 'manifest.sig').unlink()
                r = installer('--dir', str(tmp / 'bin-nosig'), *common)
                record('missing_signature_rejected', r.returncode != 0, (r.stdout + r.stderr).strip()[:200])
                (dist / 'manifest.sig').write_bytes(sig)

                link_dir = tmp / 'bin-symlink'
                link_dir.mkdir()
                (link_dir / 'mesh').symlink_to(tmp / 'elsewhere')
                r = installer('--dir', str(link_dir), *common)
                record('symlink_target_refused', r.returncode != 0, (r.stdout + r.stderr).strip()[:200])
            finally:
                proc.terminate()
                try:
                    proc.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    proc.kill()

    print('\n== summary ==')
    print(json.dumps(results, indent=2))
    print('FAILURES:', failures or 'none')
    return 1 if failures else 0


if __name__ == '__main__':
    sys.exit(main())
