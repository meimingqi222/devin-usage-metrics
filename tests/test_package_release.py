import hashlib
import importlib.util
from pathlib import Path
import tempfile
import unittest
import zipfile


spec = importlib.util.spec_from_file_location(
    "package_release", Path(__file__).resolve().parents[1] / "scripts" / "package-release.py"
)
package_release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(package_release)


class WindowsPackageTests(unittest.TestCase):
    def test_windows_branding_and_checksum(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            source = root / "build" / "windows-amd64"
            source.mkdir(parents=True)
            executable = source / "Agent Usage Metrics.exe"
            executable.write_bytes(b"packaged executable fixture")
            archive = package_release.package("windows/amd64", root / "build", root / "out")
            self.assertEqual(archive.name, "Agent-Usage-Metrics-windows-x64.zip")
            with zipfile.ZipFile(archive) as bundle:
                self.assertEqual(bundle.namelist(), ["Agent Usage Metrics.exe"])
                self.assertEqual(bundle.read("Agent Usage Metrics.exe"), executable.read_bytes())
            expected = hashlib.sha256(archive.read_bytes()).hexdigest()
            self.assertEqual(
                archive.with_suffix(".zip.sha256").read_text(encoding="utf-8"),
                f"{expected}  {archive.name}\n",
            )
            # Pre-rename clients discover only the legacy name, so the Windows
            # release publishes a byte-identical alias under it.
            legacy = root / "out" / "devin-usage-metrics-windows-x64.zip"
            self.assertEqual(legacy.read_bytes(), archive.read_bytes())
            self.assertEqual(
                legacy.with_suffix(".zip.sha256").read_text(encoding="utf-8"),
                f"{expected}  {legacy.name}\n",
            )


if __name__ == "__main__":
    unittest.main()
