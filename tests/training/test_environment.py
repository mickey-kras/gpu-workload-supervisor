"""Guard the subset boundary and exact upstream metadata deviation contract."""
import importlib.util
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, ROOT / path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


worker = load("subset_worker", "internal/llmtraining/worker.py")
checker = load("subset_checker", "training/axolotl/check_environment.py")


def distribution(name, version="1.0", requires=()):
    return SimpleNamespace(metadata={"Name": name}, version=version, requires=requires)


class SubsetTests(unittest.TestCase):
    def test_each_omission_rejects_installed_metadata_or_shadow_module(self):
        for name, module in worker.OMITTED_INTEGRATIONS.items():
            for metadata_present in (True, False):
                with self.subTest(name=name, metadata_present=metadata_present), patch.object(
                    worker.importlib.metadata, "distributions", return_value=[distribution(name.replace("-", "_"))] if metadata_present else []
                ), patch.object(worker.importlib.util, "find_spec", side_effect=lambda candidate: object() if candidate == module and not metadata_present else None):
                    with self.assertRaisesRegex(ValueError, "unapproved optional"):
                        worker.verify_subset()

    def test_clean_subset_is_accepted_without_importing_optional_code(self):
        with patch.object(worker.importlib.metadata, "distributions", return_value=[]), patch.object(worker.importlib.util, "find_spec", return_value=None):
            worker.verify_subset()

    def test_exact_upstream_deviations(self):
        requires = [name + spec for name, spec in checker.OMITTED.items()] + ["python-dotenv==1.0.1"]
        installed = [distribution("axolotl", requires=requires), distribution("python-dotenv", "1.2.2")]
        self.assertEqual(checker.deviations(installed), checker.EXPECTED)
        for changed in (
            installed + [distribution("trackio", "0.40.0")],
            installed + [distribution("unexpected", requires=["absent>=2"])],
            [distribution("axolotl", requires=requires[:-1])] + installed[1:],
            [distribution("axolotl", requires=requires + ["new-extra>=1"])] + installed[1:],
        ):
            self.assertNotEqual(checker.deviations(changed), checker.EXPECTED)

    def test_active_markers_and_transitive_conflicts_are_checked(self):
        installed = [distribution("parent", requires=["child>=2", "absent; python_version<'3'", "extra-only; extra=='optional'"]), distribution("child", "1")]
        self.assertEqual(checker.deviations(installed), {("parent", "child", ">=2", "version")})
        with self.assertRaisesRegex(ValueError, "duplicate"):
            checker.deviations([distribution("same_name"), distribution("same-name")])

    def test_requested_extras_propagate_through_cycles_and_check_versions(self):
        # Put requesters last so one forward pass cannot discover the closure.
        installed = [
            distribution("leaf", requires=["validator>=2; extra=='validate'", "inactive; extra=='unused'"]),
            distribution("middle", requires=["leaf[validate]; extra=='feature'", "root[again]; extra=='feature'"]),
            distribution("root", requires=["middle[feature]", "middle[feature]; extra=='again'", "never[unused]; python_version<'3'"]),
        ]
        expected = {("leaf", "validator", ">=2", "missing")}
        self.assertEqual(checker.deviations(installed), expected)
        self.assertEqual(checker.deviations(installed + [distribution("validator", "1")]),
                         {("leaf", "validator", ">=2", "version")})
        self.assertEqual(checker.deviations(installed + [distribution("validator", "2")]), set())

    def test_unrequested_extra_does_not_activate_its_dependency_extras(self):
        installed = [
            distribution("root", requires=["middle[feature]; extra=='optional'"]),
            distribution("middle", requires=["missing; extra=='feature'"]),
        ]
        self.assertEqual(checker.deviations(installed), set())


if __name__ == "__main__":
    unittest.main()
