"""Fail closed on every dependency deviation; do not rewrite upstream metadata."""
from collections import deque
import importlib.metadata as metadata
import importlib.util
from pathlib import Path

from packaging.requirements import Requirement
from packaging.utils import canonicalize_name

# Exact requirements from the unchanged source commit. Absence is intentional only
# for these optional integrations; python-dotenv is replaced by a patched version.
OMITTED = {
    "trackio": "==0.40.0", "gradio": "==6.29.1", "s3fs": "==2025.10.0",
    "gcsfs": "==2025.10.0", "adlfs": "==2026.2.0", "ocifs": "==1.3.2",
    "lm-eval": "==0.4.11", "openenv-core": ">=0.1.0",
}
EXPECTED = {("axolotl", name, spec, "missing") for name, spec in OMITTED.items()}
EXPECTED.add(("axolotl", "python-dotenv", "==1.0.1", "version"))


def deviations(distributions):
    installed = {}
    for distribution in distributions:
        name = canonicalize_name(distribution.metadata["Name"])
        if name in installed:
            raise ValueError("duplicate distribution: " + name)
        installed[name] = distribution
    requirements = {
        name: [Requirement(text) for text in distribution.requires or []]
        for name, distribution in installed.items()
    }
    # Every installed distribution activates its base requirements. Requested
    # extras propagate across dependency edges, including cycles, until no new
    # (distribution, extra) context remains. Unrequested extras stay inactive.
    pending = deque((name, "") for name in installed)
    visited = set()
    result = set()
    while pending:
        parent, extra = pending.popleft()
        if (parent, extra) in visited:
            continue
        visited.add((parent, extra))
        for requirement in requirements[parent]:
            if requirement.marker and not requirement.marker.evaluate({"extra": extra}):
                continue
            name = canonicalize_name(requirement.name)
            dependency = installed.get(name)
            if dependency is None:
                result.add((parent, name, str(requirement.specifier), "missing"))
            elif requirement.url or not requirement.specifier.contains(dependency.version, prereleases=True):
                result.add((parent, name, str(requirement.specifier), "version"))
            if dependency is not None:
                pending.extend((name, canonicalize_name(requested)) for requested in requirement.extras)
    return result


def main():
    actual = deviations(metadata.distributions())
    if actual != EXPECTED:
        raise ValueError(f"dependency deviations changed: missing={sorted(EXPECTED-actual)}, unexpected={sorted(actual-EXPECTED)}")
    for name, floor in {"python-dotenv": ">=1.2.2", "fsspec": ">=2026.6.0", "pillow": ">=13"}.items():
        if not Requirement(name + floor).specifier.contains(metadata.version(name)):
            raise ValueError("security floor not met: " + name)
    path = Path(__file__).resolve().parents[2] / "internal/llmtraining/worker.py"
    spec = importlib.util.spec_from_file_location("training_worker", path)
    worker = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(worker)
    worker.verify_environment()
    print("Verified exactly eight omitted requirements and one patched dotenv requirement")


if __name__ == "__main__":
    main()
