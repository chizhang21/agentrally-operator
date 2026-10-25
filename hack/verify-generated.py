"""Check regeneration deterministically, including untracked generated files."""
from pathlib import Path
import hashlib
import os
import shutil
import subprocess
import tempfile

root = Path(__file__).resolve().parents[1]

def snapshot(directory):
    patterns = ['api/**/zz_generated.*.go', 'chart/templates/crds/*.yaml',
                'chart/templates/roles.yaml']
    return {str(p.relative_to(directory)): hashlib.sha256(p.read_bytes()).hexdigest()
            for pattern in patterns for p in directory.glob(pattern)}

before = snapshot(root)
with tempfile.TemporaryDirectory(prefix='agentrally-generated-') as temporary:
    stage = Path(temporary)
    for name in ('api', 'internal', 'cmd', 'hack', 'mk', 'Makefile', 'go.mod', 'go.sum'):
        source = root / name
        if source.is_dir():
            shutil.copytree(source, stage / name,
                            ignore=shutil.ignore_patterns('.cache', '.work', '__pycache__'))
        elif source.is_file():
            shutil.copy2(source, stage / name)
    tools = Path(os.environ.get('LOCALBIN', root / '.cache/tools')).resolve()
    subprocess.run(['make', 'generate', f'LOCALBIN={tools}'], cwd=stage, check=True)
    after = snapshot(stage)
changed = sorted(name for name in before.keys() | after.keys() if before.get(name) != after.get(name))
if changed:
    raise SystemExit('Generated files need updating:\n' + '\n'.join(changed))
print('Generated DeepCopy, CRDs and RBAC are current')
