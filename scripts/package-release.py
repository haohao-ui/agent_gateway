#!/usr/bin/env python3
"""Package signed files; release verify must succeed before invoking this script."""
import json
from pathlib import Path
import re
import sys
import tarfile
import zipfile

root = Path(sys.argv[1])
version = sys.argv[2]
if not re.fullmatch(r'[A-Za-z0-9._-]+', version):
    raise SystemExit('Invalid release version')
manifest = json.loads((root/'manifest.json').read_text())
common = ['manifest.json','manifest.sig','LICENSE','NOTICE','THIRD_PARTY_NOTICES','README.md']
for target, entry in manifest['files'].items():
    if target in ['LICENSE','NOTICE','THIRD_PARTY_NOTICES']:
        continue
    if not re.fullmatch(r'(darwin|linux|windows)-(amd64|arm64)', target):
        raise SystemExit('Unexpected target')
    name = entry['name']
    expected = 'mesh-' + target + ('.exe' if target.startswith('windows-') else '')
    if name != expected:
        raise SystemExit('Unexpected binary path')
    filenames = [name, *common]
    if target.startswith('windows-'):
        with zipfile.ZipFile(root/f'agent-gateway_{version}_{target}.zip','x',zipfile.ZIP_DEFLATED) as archive:
            for item in filenames:
                archive.write(root/item,item)
    else:
        with tarfile.open(root/f'agent-gateway_{version}_{target}.tar.gz','x:gz') as archive:
            for item in filenames:
                archive.add(root/item,arcname=item,recursive=False)
