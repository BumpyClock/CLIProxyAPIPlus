#!/usr/bin/env python3
"""Offline executable Go hook fixtures. Never runs an outbound push or server."""
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]


class HookTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="go push ")
        self.addCleanup(self.temp.cleanup)
        self.parent = Path(self.temp.name)
        self.repo = self.parent / "checkout with spaces"
        self.repo.mkdir()
        self.git("init", "-q")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "user.name", "Fixture")
        for path in [".githooks/pre-push", "scripts/install-git-hooks.sh"]:
            dest = self.repo / path
            dest.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(ROOT / path, dest)
        if (ROOT / '.gitattributes').exists():
            shutil.copy2(ROOT / '.gitattributes', self.repo / '.gitattributes')
        (self.repo / "file with spaces.go").write_text("package fixture\n")
        (self.repo / "go.mod").write_text("module fixture\ngo 1.26.0\n")
        self.git("add", "-A")
        self.git("commit", "-qm", "base")
        self.base = self.git("rev-parse", "HEAD")
        (self.repo / "file with spaces.go").write_text("package renamed\n")
        self.git("commit", "-qam", "update")
        self.head = self.git("rev-parse", "HEAD")
        self.zero = "0" * len(self.head)
        self.calls = self.parent / "calls"
        self.bin = self.parent / "bin"
        self.bin.mkdir()
        for name, text in {
            "go": '#!/bin/sh\necho "go $1" >> "$CALLS"\nif [ "$1" = "$FAIL_GO" ]; then exit 23; fi\n',
            "gofmt": '#!/bin/sh\necho "format $3" >> "$CALLS"\nif [ "$BAD_FORMAT" = 1 ]; then echo "$3"; fi\n',
        }.items():
            file = self.bin / name
            file.write_text(text)
            file.chmod(0o755)

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.repo, text=True).strip()

    def record(self, local=None, remote=None):
        return f"refs/heads/topic {local or self.head} refs/heads/topic {remote or self.base}\n"

    def hook(self, records=None, **extra):
        self.calls.write_text("")
        result = subprocess.run([str(self.repo / ".githooks/pre-push")], cwd=self.repo,
                                input=self.record() if records is None else records, text=True,
                                env={**os.environ, "PATH": f"{self.bin}:{os.environ['PATH']}",
                                     "CALLS": str(self.calls), "BAD_FORMAT": "0", "FAIL_GO": "", **extra},
                                capture_output=True)
        return result, self.calls.read_text().splitlines()

    def test_new_unknown_empty_and_multi_ref_ranges(self):
        for records in [self.record(), self.record(remote=self.zero), self.record(remote="f" * 40),
                        self.record(remote=self.head), "", self.record() + self.record(remote=self.zero)]:
            result, calls = self.hook(records)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(calls[-2:], ["go build", "go vet"])

    def test_rename_delete_and_config_fallback(self):
        self.git("mv", "file with spaces.go", "renamed.go")
        self.git("commit", "-qm", "rename")
        self.head = self.git("rev-parse", "HEAD")
        self.assertIn("format renamed.go", self.hook()[1])
        self.git("rm", "renamed.go")
        self.git("commit", "-qm", "delete")
        self.head = self.git("rev-parse", "HEAD")
        self.assertEqual(self.hook()[1], ["go build", "go vet"])
        (self.repo / "unchanged.go").write_text("package fixture\n")
        self.git("add", "unchanged.go")
        self.git("commit", "-qm", "source")
        self.base = self.git("rev-parse", "HEAD")
        (self.repo / "go.mod").write_text("module fixture\ngo 1.26.1\n")
        self.git("commit", "-qam", "config")
        self.head = self.git("rev-parse", "HEAD")
        self.assertIn("format unchanged.go", self.hook()[1])

    def test_deletion_only_and_non_head_malformed_dirty(self):
        self.assertEqual(self.hook(self.record(local=self.zero))[1], [])
        self.assertNotEqual(self.hook(self.record(local=self.base))[0].returncode, 0)
        self.assertNotEqual(self.hook("bad record\n")[0].returncode, 0)
        (self.repo / "user-work").write_text("preserve")
        self.assertNotEqual(self.hook()[0].returncode, 0)

    def test_format_build_and_vet_failures(self):
        self.assertNotEqual(self.hook(BAD_FORMAT="1")[0].returncode, 0)
        self.assertEqual(self.hook(FAIL_GO="build")[0].returncode, 23)
        self.assertEqual(self.hook(FAIL_GO="vet")[0].returncode, 23)

    def test_installer_idempotent_and_refuses_active_hook_or_config(self):
        def install():
            return subprocess.run([str(self.repo / "scripts/install-git-hooks.sh")], cwd=self.repo,
                                  capture_output=True).returncode
        active = self.repo / ".git/hooks/pre-commit"
        active.write_text("#!/bin/sh\nexit 0\n")
        active.chmod(0o755)
        self.assertNotEqual(install(), 0)
        self.assertTrue(active.exists())
        active.unlink()
        self.git("config", "core.hooksPath", "unrelated hooks")
        self.assertNotEqual(install(), 0)
        self.assertEqual(self.git("config", "core.hooksPath"), "unrelated hooks")
        self.git("config", "--unset", "core.hooksPath")
        self.assertEqual(install(), 0)
        self.assertEqual(install(), 0)
        self.assertEqual(self.git("config", "core.hooksPath"), ".githooks")

    def test_git_status_error_stops_before_project_checks(self):
        (self.repo / '.git/index').write_bytes(b'bad')
        result, calls = self.hook()
        self.assertNotEqual(result.returncode, 0, result.stderr)
        self.assertEqual(calls, [])

    def test_autocrlf_checkout_preserves_shell_entrypoints(self):
        self.git('config', 'core.autocrlf', 'true')
        self.git('checkout-index', '--force', '--all')
        for path in ['.githooks/pre-push', 'scripts/install-git-hooks.sh']:
            self.assertNotIn(b'\r\n', (self.repo / path).read_bytes(), path)


if __name__ == "__main__":
    unittest.main()
