"""Exercise integration refusal before mutation using pristine retained source."""
import hashlib
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent
PRISTINE = pathlib.Path(sys.argv.pop(1)).resolve()
PINNED = [line.split('  ', 1)[1] for line in
          (ROOT / 'patches/talos-1.14.1-source.sha256').read_text().splitlines()]


class InstallBoundaries(unittest.TestCase):
    def fixture(self, root):
        for name in PINNED:
            dst = root / name
            dst.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(PRISTINE / name, dst)

    def snapshot(self, root):
        return {str(p.relative_to(root)): hashlib.sha256(p.read_bytes()).hexdigest()
                for p in root.rglob('*') if p.is_file()}

    def run_install(self, root):
        return subprocess.run(['bash', str(ROOT / 'install.sh'), str(root)],
                              capture_output=True, text=True)

    def test_wrong_source_refuses_before_any_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            self.fixture(root)
            p = root / PINNED[0]
            p.write_text(p.read_text() + '\n// changed upstream\n')
            before = self.snapshot(root)
            self.assertNotEqual(self.run_install(root).returncode, 0)
            self.assertEqual(self.snapshot(root), before)

    def test_existing_overlay_refuses_before_any_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            self.fixture(root)
            p = root / 'pkg/machinery/bootinventory/inventory.go'
            p.parent.mkdir(parents=True)
            p.write_text('existing writer bytes')
            before = self.snapshot(root)
            self.assertNotEqual(self.run_install(root).returncode, 0)
            self.assertEqual(self.snapshot(root), before)

    def test_exact_integration_and_second_application_refusal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            self.fixture(root)
            result = self.run_install(root)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn('bool efi_inventory = 2;',
                          (root / 'api/machine/machine.proto').read_text())
            before = self.snapshot(root)
            self.assertNotEqual(self.run_install(root).returncode, 0)
            self.assertEqual(self.snapshot(root), before)


if __name__ == '__main__':
    unittest.main()
