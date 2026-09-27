#!/usr/bin/env python3
"""Collect full license/notice files of modules linked on supported targets.
Runs only Go metadata queries; no secrets, network credential config or services.
"""
import json
import os
from pathlib import Path
import subprocess

root = Path(__file__).resolve().parents[1]

def objects(raw):
    decoder = json.JSONDecoder()
    while raw.strip():
        obj, end = decoder.raw_decode(raw.lstrip())
        raw = raw.lstrip()[end:]
        yield obj

modules = {}
for target in ['darwin/arm64','darwin/amd64','linux/amd64','linux/arm64','windows/amd64','windows/arm64']:
    goos, goarch = target.split('/')
    env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED='0')
    raw = subprocess.check_output(['go','list','-deps','-json','./cmd/mesh'],cwd=root,env=env,text=True)
    for package in objects(raw):
        module = package.get('Module', {})
        if module and not module.get('Main'):
            modules[module['Path']] = module

out = ['Agent Gateway — Third-party license and notice texts',
       'Generated from linked Go module metadata for six supported build targets.',
       'Includes full upstream license/notice files, including bundled components.',
       'Regenerate after dependency/toolchain changes; verify the final release bundle.', '']
for name, module in sorted(modules.items()):
    directory = Path(module['Dir'])
    files = sorted(p for p in directory.rglob('*') if p.is_file() and
                   ('license' in p.name.lower() or p.name.lower() in ['notice','copying','copyright']))
    if not files:
        raise SystemExit('Missing license material: ' + name)
    out.extend(['=' * 72, name + ' ' + module['Version']])
    for path in files:
        out.extend(['--- ' + path.relative_to(directory).as_posix() + ' ---', path.read_text(encoding='utf-8'), ''])
goro = Path(subprocess.check_output(['go','env','GOROOT'],cwd=root,text=True).strip())
out.extend(['='*72, 'Go toolchain/runtime ' + subprocess.check_output(['go','version'],text=True).strip(), (goro/'LICENSE').read_text()])
for path in sorted((goro/'src/vendor').rglob('LICENSE*')):
    if path.is_file():
        out.extend(['--- Go ' + path.relative_to(goro).as_posix() + ' ---',path.read_text(), ''])
(root/'THIRD_PARTY_NOTICES').write_text('\n'.join(out).rstrip() + '\n',encoding='utf-8')
print('Collected license/notice files for',len(modules),'linked modules and Go runtime/vendor sources.')
