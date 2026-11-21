import importlib.util
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("check_contracts", ROOT / "tools/check_contracts.py")
contracts = importlib.util.module_from_spec(spec)
spec.loader.exec_module(contracts)


class DesignDocumentSelection(unittest.TestCase):
    def test_ignores_guest_and_generated_trees_but_checks_source_documents(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for filename in ("README.md", "docs/guide.md", "fixtures/README.md",
                             ".local/build/root/guest.md", ".git/README.md", "dist/generated.md", "bin/generated.md"):
                path = root / filename
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text("[broken](missing-file)\n")
            paths = {str(path.relative_to(root)) for path in contracts.markdown_files(root)}
            self.assertEqual(paths, {"README.md", "docs/guide.md", "fixtures/README.md"})
