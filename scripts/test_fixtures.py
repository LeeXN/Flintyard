import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tarfile
import tempfile
import unittest
import urllib.error
from unittest import mock

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("fixture_prepare", ROOT / "fixtures/prepare.py")
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)
sys.path.insert(0, str(ROOT / "fixtures"))
config_spec = importlib.util.spec_from_file_location("fixture_config", ROOT / "fixtures/configure-kernel.py")
configure = importlib.util.module_from_spec(config_spec)
config_spec.loader.exec_module(configure)
render_spec = importlib.util.spec_from_file_location("fixture_render", ROOT / "fixtures/render.py")
render = importlib.util.module_from_spec(render_spec)
render_spec.loader.exec_module(render)
bundle_spec = importlib.util.spec_from_file_location("alpine_bundle", ROOT / "fixtures/alpine-bundle.py")
alpine_bundle = importlib.util.module_from_spec(bundle_spec)
bundle_spec.loader.exec_module(alpine_bundle)
fetch_spec = importlib.util.spec_from_file_location("fetch_alpine", ROOT / "fixtures/fetch-alpine.py")
fetch_alpine = importlib.util.module_from_spec(fetch_spec)
fetch_spec.loader.exec_module(fetch_alpine)
cloud_spec = importlib.util.spec_from_file_location("debian_cloud", ROOT / "fixtures/debian-cloud.py")
debian_cloud = importlib.util.module_from_spec(cloud_spec)
cloud_spec.loader.exec_module(debian_cloud)
cloud_render_spec = importlib.util.spec_from_file_location("render_debian_cloud", ROOT / "fixtures/render-debian-cloud.py")
cloud_render = importlib.util.module_from_spec(cloud_render_spec)
cloud_render_spec.loader.exec_module(cloud_render)
ubuntu_original_spec = importlib.util.spec_from_file_location("ubuntu_original", ROOT / "fixtures/ubuntu-original.py")
ubuntu_original = importlib.util.module_from_spec(ubuntu_original_spec)
ubuntu_original_spec.loader.exec_module(ubuntu_original)
ubuntu_render_spec = importlib.util.spec_from_file_location("render_ubuntu_original", ROOT / "fixtures/render-ubuntu-original.py")
ubuntu_render = importlib.util.module_from_spec(ubuntu_render_spec)
ubuntu_render_spec.loader.exec_module(ubuntu_render)
sys.path.pop(0)


class FixtureInputs(unittest.TestCase):
    def test_lock_covers_four_families_both_architectures_without_placeholders(self):
        lock = prepare.strict_json((ROOT / "fixtures/lock.json").read_bytes())
        self.assertEqual(set(lock["families"]), {"ubuntu", "debian", "rocky", "alpine"})
        for family in lock["families"].values():
            self.assertEqual(set(family["arches"]), {"x86_64", "aarch64"})
            for source in family["arches"].values():
                self.assertTrue(source["url"].startswith("https://"))
                self.assertNotIn("latest", source["url"])
                self.assertRegex(source["sha256"], "^[0-9a-f]{64}$")
                self.assertNotEqual(source["sha256"], "0" * 64)
                self.assertGreater(source["size_bytes"], 0)
                self.assertFalse(source["signature_verified"])
                if "normalization" in source:
                    normalized = source["normalization"]
                    self.assertEqual(normalized["format"], "tar-gzip-v1")
                    self.assertRegex(normalized["sha256"], "^[0-9a-f]{64}$")
                    self.assertRegex(normalized["tar_sha256"], "^[0-9a-f]{64}$")
                    self.assertTrue(normalized["drop_devices"])
        for config in lock["kernel"]["config_templates"].values():
            self.assertEqual(prepare.digest(ROOT / "fixtures" / config["path"]), config["sha256"])
        package_data = (ROOT / "fixtures/alpine-packages.lock.json").read_bytes()
        self.assertEqual(hashlib.sha256(package_data).hexdigest(), lock["families"]["alpine"]["offline_package_lock_sha256"])
        package_lock = prepare.strict_json(package_data)
        for arch, repos in package_lock["indexes"].items():
            for repo, index in repos.items():
                path = ROOT / "fixtures/alpine-indexes" / arch / repo / "APKINDEX.tar.gz"
                self.assertEqual((path.stat().st_size, prepare.digest(path)), (index["size"], index["sha256"]))
        cloud_data = (ROOT / "fixtures/debian-cloud.lock.json").read_bytes()
        self.assertEqual(hashlib.sha256(cloud_data).hexdigest(), lock["families"]["debian"]["cloud_lock_sha256"])
        ubuntu_data = (ROOT / "fixtures/ubuntu-original.lock.json").read_bytes()
        self.assertEqual(hashlib.sha256(ubuntu_data).hexdigest(), lock["families"]["ubuntu"]["original_kernel_lock_sha256"])
        rocky_data = (ROOT / "fixtures/rocky-original-kernel.lock.json").read_bytes()
        self.assertEqual(hashlib.sha256(rocky_data).hexdigest(), lock["families"]["rocky"]["original_kernel_lock_sha256"])
        rocky_kernel = prepare.strict_json(rocky_data)
        self.assertEqual(set(rocky_kernel["architectures"]), {"x86_64", "aarch64"})
        for arch, entry in rocky_kernel["architectures"].items():
            self.assertTrue(entry["kernel_release"].endswith("." + arch))
            for field in ("package_sha256", "packaged_kernel_sha256", "runtime_kernel_sha256", "config_sha256"):
                self.assertRegex(entry[field], "^[0-9a-f]{64}$")

    def archive(self, directory, mode=None):
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode="w") as archive:
            content = b"fixture root data"
            info = tarfile.TarInfo("etc/fixture")
            info.size = len(content)
            archive.addfile(info, io.BytesIO(content))
        layer = gzip.compress(raw.getvalue(), mtime=0)
        layer_sha = hashlib.sha256(layer).hexdigest()
        diff = hashlib.sha256(raw.getvalue()).hexdigest()
        config = json.dumps({"architecture": "amd64", "os": "linux", "rootfs": {"diff_ids": ["sha256:" + diff]}}).encode()
        config_sha = hashlib.sha256(config).hexdigest()
        manifest = json.dumps({"schemaVersion": 2, "config": {"digest": "sha256:" + config_sha, "size": len(config)},
                               "layers": [{"mediaType": "application/vnd.oci.image.layer.v1.tar+gzip", "digest": "sha256:" + layer_sha, "size": len(layer)}]}).encode()
        manifest_sha = hashlib.sha256(manifest).hexdigest()
        index = json.dumps({"schemaVersion": 2, "manifests": [{"digest": "sha256:" + manifest_sha, "size": len(manifest)}]}).encode()
        members = {"oci-layout": b'{"imageLayoutVersion":"1.0.0"}', "index.json": index,
                   "blobs/sha256/" + manifest_sha: manifest, "blobs/sha256/" + config_sha: config,
                   "blobs/sha256/" + layer_sha: layer}
        path = directory / "fixture.oci.tar.xz"
        with tarfile.open(path, "w:xz") as archive:
            for name, data in members.items():
                info = tarfile.TarInfo(name)
                info.size = len(data)
                archive.addfile(info, io.BytesIO(data))
            if mode:
                info = tarfile.TarInfo({"parent": "../outside", "absolute": "/outside", "duplicate": "index.json", "link": "blobs/sha256/" + "f" * 64}[mode])
                if mode == "link":
                    info.type = tarfile.SYMTYPE
                    info.linkname = "/outside"
                archive.addfile(info, io.BytesIO(b""))
        return path, layer_sha, diff, layer

    def test_single_layer_copy_is_complete_and_architecture_checked(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            path, layer_sha, diff, expected = self.archive(directory)
            out = directory / "rootfs.tar.gz"
            prepare.oci_single_layer(path, out, "x86_64", layer_sha, diff)
            self.assertEqual(out.read_bytes(), expected)
            with self.assertRaises(ValueError):
                prepare.oci_single_layer(path, directory / "wrong-arch", "aarch64", layer_sha, diff)
            with self.assertRaises(ValueError):
                prepare.oci_single_layer(path, directory / "wrong-hash", "x86_64", "a" * 64, diff)

    def test_outer_archive_cannot_escape_link_or_duplicate(self):
        for mode in ("parent", "absolute", "duplicate", "link"):
            with self.subTest(mode=mode), tempfile.TemporaryDirectory() as directory:
                directory = Path(directory)
                path, layer_sha, diff, _ = self.archive(directory, mode)
                with self.assertRaises(ValueError):
                    prepare.oci_single_layer(path, directory / "rootfs", "x86_64", layer_sha, diff)
                self.assertFalse((directory / "rootfs").exists())

    def test_json_duplicate_or_non_utf8_is_not_accepted(self):
        for data in (b'{"schemaVersion":2,"schemaVersion":1}', b'{"key":"\xff"}'):
            with self.assertRaises((ValueError, UnicodeDecodeError)):
                prepare.strict_json(data)

    def test_uncompressed_identity_is_stable_across_gzip_headers(self):
        with tempfile.TemporaryDirectory() as directory:
            first, second = Path(directory) / "one.gz", Path(directory) / "two.gz"
            first.write_bytes(gzip.compress(b"locked tar bytes", mtime=0))
            second.write_bytes(gzip.compress(b"locked tar bytes", mtime=1))
            self.assertNotEqual(prepare.digest(first), prepare.digest(second))
            self.assertEqual(prepare.gzip_content_digest(first), prepare.gzip_content_digest(second))

    def test_config_publication_preserves_existing_provenance(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            work = directory / "work"
            work.mkdir()
            staged = work / ".config"
            staged.write_text("locked config")
            output = directory / "kernel.config"
            report = Path(str(output) + ".json")
            report.write_text("existing report")
            with self.assertRaises(FileExistsError):
                configure.publish_config(staged, {"config_sha256": "test"}, output)
            self.assertEqual(report.read_text(), "existing report")
            self.assertFalse(output.exists())

    def test_recipe_is_complete_and_does_not_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "recipe.json"
            render.publish_recipe(output, {"architecture": "x86_64"})
            self.assertEqual(json.loads(output.read_text()), {"architecture": "x86_64"})
            with self.assertRaises(FileExistsError):
                render.publish_recipe(output, {"architecture": "aarch64"})
            self.assertEqual(json.loads(output.read_text()), {"architecture": "x86_64"})

    def test_ubuntu_recipe_declares_root_and_disables_host_swap_detection(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            rootfs, kernel, config = (directory / name for name in ("rootfs", "source", "config"))
            rootfs.write_bytes(b"root archive")
            kernel.write_bytes(b"kernel archive")
            config.write_bytes(b"kernel config")
            release = "6.1.167"
            Path(str(config) + ".json").write_text(json.dumps({
                "architecture": "x86_64", "kernel_release": release,
                "config_sha256": prepare.digest(config), "modules": "enabled"}))
            lock = {"kernel": {"sha256": prepare.digest(kernel), "release": release},
                    "families": {"ubuntu": {"release": "24.04", "profile": "systemd-networkd",
                                             "arches": {"x86_64": {"rootfs_sha256": prepare.digest(rootfs)}}}}}
            (directory / "lock.json").write_text(json.dumps(lock))
            recipe_path = directory / "recipe.json"
            with mock.patch.object(render, "BASE", directory):
                render.render("ubuntu", "x86_64", rootfs, kernel, config, directory / "out.fyimg", recipe_path)
            recipe = json.loads(recipe_path.read_text())
            commands = [step["argv"] for step in recipe["steps"]]
            fstab = next(i for i, cmd in enumerate(commands) if "/dev/vda / ext4" in " ".join(cmd))
            no_resume = next(i for i, cmd in enumerate(commands) if "RESUME=none" in " ".join(cmd))
            initramfs = next(i for i, cmd in enumerate(commands) if cmd[0] == "update-initramfs")
            self.assertLess(fstab, initramfs)
            self.assertLess(no_resume, initramfs)
            self.assertFalse(recipe["network"])

    def test_rocky_recipe_explicitly_prepares_pam_for_locked_accounts(self):
        for architecture in ("x86_64", "aarch64"):
            with self.subTest(architecture=architecture), tempfile.TemporaryDirectory() as directory:
                directory = Path(directory)
                rootfs, kernel, config = (directory / name for name in ("rootfs", "source", "config"))
                rootfs.write_bytes(b"root archive")
                kernel.write_bytes(b"kernel archive")
                config.write_bytes(b"kernel config")
                release = "6.1.167"
                Path(str(config) + ".json").write_text(json.dumps({"architecture": architecture,
                    "kernel_release": release, "config_sha256": prepare.digest(config), "modules": "disabled"}))
                lock = {"kernel": {"sha256": prepare.digest(kernel), "release": release},
                        "families": {"rocky": {"release": "9.8", "profile": "networkmanager",
                                              "arches": {architecture: {"rootfs_sha256": prepare.digest(rootfs)}}}}}
                (directory / "lock.json").write_text(json.dumps(lock))
                recipe_path = directory / "recipe.json"
                with mock.patch.object(render, "BASE", directory):
                    render.render("rocky", architecture, rootfs, kernel, config, directory / "out.fyimg", recipe_path)
                recipe = json.loads(recipe_path.read_text())
                commands = [" ".join(step["argv"]) for step in recipe["steps"]]
                self.assertTrue(any("UsePAM yes" in command and "/etc/ssh/sshd_config" in command for command in commands))
                self.assertEqual(recipe["guest_prepare"]["profile"], "networkmanager")
                self.assertEqual(recipe["reproducibility"], "best_effort")

    def test_offline_alpine_bundle_checks_all_inputs_and_reserved_paths(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            source = directory / "root.tar.gz"
            with tarfile.open(source, "w:gz") as archive:
                for name in ("var", "var/lib"):
                    info = tarfile.TarInfo(name)
                    info.type = tarfile.DIRTYPE
                    archive.addfile(info)
            root = directory / "packages"
            indexes = {}
            for repo in ("main", "community"):
                folder = root / "x86_64" / repo
                folder.mkdir(parents=True)
                data = (repo + "-index").encode()
                (folder / "APKINDEX.tar.gz").write_bytes(data)
                indexes[repo] = {"size": len(data), "sha256": hashlib.sha256(data).hexdigest()}
            package = b"signed package fixture"
            path = root / "x86_64/main/one-1-r0.apk"
            path.write_bytes(package)
            lock = {"indexes": {"x86_64": indexes}, "packages": {"x86_64": [{"repository": "main", "name": "one", "version": "1-r0", "size": len(package), "sha256": hashlib.sha256(package).hexdigest()}]}}
            first, second = directory / "first.gz", directory / "second.gz"
            for output in (first, second):
                alpine_bundle.bundle(source, output, "x86_64", lock, root)
            self.assertEqual(first.read_bytes(), second.read_bytes())
            with tarfile.open(first, "r:gz") as archive:
                self.assertEqual(archive.extractfile("var/lib/flintyard-fixture-apk/main/x86_64/one-1-r0.apk").read(), package)
                self.assertIn(b"file:///var/lib/flintyard-fixture-apk/main", archive.extractfile("var/lib/flintyard-fixture-apk/repositories").read())
            path.write_bytes(b"x" * len(package))
            with self.assertRaisesRegex(ValueError, "digest"):
                alpine_bundle.bundle(source, directory / "invalid.gz", "x86_64", lock, root)
            with tarfile.open(source, "w:gz") as archive:
                for name in ("var", "var/lib", "var/lib/flintyard-fixture-apk"):
                    info = tarfile.TarInfo(name)
                    info.type = tarfile.DIRTYPE
                    archive.addfile(info)
            with self.assertRaisesRegex(ValueError, "reserved"):
                alpine_bundle.bundle(source, directory / "reserved.gz", "x86_64", lock, root)

    def test_offline_alpine_recipe_uses_only_locked_local_packages(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            rootfs, kernel, config = (directory / name for name in ("rootfs", "source", "config"))
            rootfs.write_bytes(b"offline root archive")
            kernel.write_bytes(b"kernel archive")
            config.write_bytes(b"kernel config")
            release = "6.1.167"
            Path(str(config) + ".json").write_text(json.dumps({"architecture": "x86_64", "kernel_release": release,
                                                               "config_sha256": prepare.digest(config), "modules": "disabled"}))
            names = ("openrc", "openssh-server", "iproute2", "util-linux", "coreutils", "shadow", "busybox-openrc", "busybox-mdev-openrc")
            package_data = json.dumps({"packages": {"x86_64": [{"name": n, "version": "1-r0"} for n in names]}}).encode()
            (directory / "alpine-packages.lock.json").write_bytes(package_data)
            lock = {"kernel": {"sha256": prepare.digest(kernel), "release": release}, "families": {"alpine": {
                "release": "3.22.8", "profile": "openrc-ifupdown", "offline_package_lock_sha256": hashlib.sha256(package_data).hexdigest(),
                "arches": {"x86_64": {"rootfs_sha256": "1" * 64, "offline_rootfs_sha256": prepare.digest(rootfs)}}}}}
            (directory / "lock.json").write_text(json.dumps(lock))
            recipe = directory / "recipe.json"
            with mock.patch.object(render, "BASE", directory):
                render.render("alpine", "x86_64", rootfs, kernel, config, directory / "out.fyimg", recipe)
            result = json.loads(recipe.read_text())
            self.assertFalse(result["network"])
            self.assertEqual(result["reproducibility"], "inputs_locked")
            self.assertEqual(result["steps"][0]["argv"][:3], ["apk", "add", "--no-network"])
            self.assertIn("openssh-server=1-r0", result["steps"][0]["argv"])
            tar_bytes = b"identical normalized tar stream"
            canonical = gzip.compress(tar_bytes, mtime=0)
            rootfs.write_bytes(gzip.compress(tar_bytes, mtime=1))
            entry = lock["families"]["alpine"]["arches"]["x86_64"]
            entry["offline_rootfs_sha256"] = hashlib.sha256(canonical).hexdigest()
            entry["offline_rootfs_tar_sha256"] = hashlib.sha256(tar_bytes).hexdigest()
            (directory / "lock.json").write_text(json.dumps(lock))
            alternate_recipe = directory / "alternate.json"
            with mock.patch.object(render, "BASE", directory):
                render.render("alpine", "x86_64", rootfs, kernel, config, directory / "alternate.fyimg", alternate_recipe)
            alternate = json.loads(alternate_recipe.read_text())
            self.assertEqual(alternate["rootfs"]["source"]["sha256"], prepare.digest(rootfs))
            self.assertFalse(alternate["network"])

    def test_alpine_fetch_publishes_verified_bytes_without_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "package.apk"
            content = b"locked package"
            digest = hashlib.sha256(content).hexdigest()
            self.assertEqual(fetch_alpine.publish(path, digest, len(content), lambda: io.BytesIO(content)), "downloaded")
            self.assertEqual(fetch_alpine.publish(path, digest, len(content), lambda: self.fail("unexpected network access")), "reused")
            with self.assertRaises(ValueError):
                fetch_alpine.publish(path, "f" * 64, len(content), lambda: io.BytesIO(content))
            self.assertEqual(path.read_bytes(), content)
            other = Path(directory) / "bad.apk"
            with self.assertRaises(ValueError):
                fetch_alpine.publish(other, digest, len(content), lambda: io.BytesIO(b"wrong content!"))
            self.assertFalse(other.exists())
            link = Path(directory) / "link.apk"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                fetch_alpine.publish(link, digest, len(content), lambda: io.BytesIO(content))
            with self.assertRaises(ValueError):
                fetch_alpine.OfficialRedirect().redirect_request(None, None, 302, "", {}, "http://evil.invalid/package.apk")
            operation = mock.Mock(side_effect=[urllib.error.URLError("temporary"), "downloaded"])
            with mock.patch.object(fetch_alpine.time, "sleep"):
                self.assertEqual(fetch_alpine.retry_fetch(operation), "downloaded")
            self.assertEqual(operation.call_count, 2)
            operation = mock.Mock(side_effect=ValueError("digest mismatch"))
            with self.assertRaises(ValueError):
                fetch_alpine.retry_fetch(operation)
            self.assertEqual(operation.call_count, 1)

    def test_cloud_sparse_and_partition_bounds_fail_closed(self):
        member = tarfile.TarInfo("disk.raw")
        member.size = 100
        member.sparse = [(0, 20), (30, 70)]
        debian_cloud.validate_sparse(member, 100)
        for sparse in ([(0, 50), (49, 1)], [(0, 101)], [(-1, 1)], [(0, 0)]):
            member.sparse = sparse
            with self.assertRaises(ValueError):
                debian_cloud.validate_sparse(member, 100)
        member.sparse = []
        member.type = tarfile.SYMTYPE
        with self.assertRaises(ValueError):
            debian_cloud.validate_sparse(member, 100)
        entry = {"disk_size": 8192, "partition_type": "ROOT", "partition_uuid": "ID", "partition_offset": 512, "partition_size": 1024}
        table = {"label": "gpt", "sectorsize": 512, "partitions": [{"start": 1, "size": 2, "type": "ROOT", "uuid": "ID"}, {"start": 4, "size": 2, "type": "OTHER"}]}
        self.assertEqual(debian_cloud.validate_partition(table, entry), (512, 1024))
        table["partitions"][1]["start"] = 2
        with self.assertRaisesRegex(ValueError, "overlap"):
            debian_cloud.validate_partition(table, entry)

    def test_cloud_output_directory_publication_never_replaces_existing(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            staged, existing = directory / "staged", directory / "existing"
            staged.mkdir(); existing.mkdir()
            (existing / "sentinel").write_text("retain")
            with self.assertRaises(FileExistsError):
                debian_cloud.publish_directory(staged, existing)
            self.assertEqual((existing / "sentinel").read_text(), "retain")
            output = directory / "output"
            debian_cloud.publish_directory(staged, output)
            self.assertTrue(output.is_dir())

    def test_debian_cloud_recipe_preserves_locked_distribution_boot_files(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            input_dir = directory / "input"
            input_dir.mkdir()
            for name in ("rootfs.raw", "kernel", "initrd", "config"):
                (input_dir / name).write_bytes((name + " fixture").encode())
            entry = {"rootfs_sha256": prepare.digest(input_dir / "rootfs.raw"), "partition_size": (input_dir / "rootfs.raw").stat().st_size,
                     "kernel_release": "6.12.115-cloud-amd64", "boot_files": {}}
            for name in ("kernel", "initrd", "config"):
                entry["boot_files"][name] = {"sha256": prepare.digest(input_dir / name), "size": (input_dir / name).stat().st_size}
            (input_dir / "provenance.json").write_text(json.dumps({"architecture": "x86_64", "rootfs_sha256": entry["rootfs_sha256"], "kernel_release": entry["kernel_release"]}))
            cloud_data = json.dumps({"architectures": {"x86_64": entry}}).encode()
            (directory / "debian-cloud.lock.json").write_bytes(cloud_data)
            (directory / "lock.json").write_text(json.dumps({"families": {"debian": {"cloud_lock_sha256": hashlib.sha256(cloud_data).hexdigest()}}}))
            path = directory / "recipe.json"
            with mock.patch.object(cloud_render, "BASE", directory):
                cloud_render.recipe("x86_64", input_dir, directory / "out.fyimg", path)
            result = json.loads(path.read_text())
            self.assertEqual((result["kernel"]["origin"], result["kernel"]["modules"]), ("distribution-binary", "rootfs"))
            self.assertEqual(result["initrd"]["sha256"], entry["boot_files"]["initrd"]["sha256"])
            self.assertFalse(result["network"])
            (input_dir / "initrd").write_bytes(b"tampered")
            with mock.patch.object(cloud_render, "BASE", directory), self.assertRaises(ValueError):
                cloud_render.recipe("x86_64", input_dir, directory / "other.fyimg", directory / "other.json")

    def test_ubuntu_original_overlay_confines_selected_package_paths(self):
        release = "6.8.0-147-generic"
        name = ubuntu_original.member_name("./lib/modules/" + release + "/kernel/driver.ko")
        self.assertEqual(ubuntu_original.package_target(name, release), "usr/lib/modules/" + release + "/kernel/driver.ko")
        self.assertEqual(ubuntu_original.package_target("boot/vmlinuz-" + release, release), "boot/vmlinuz-" + release)
        self.assertIsNone(ubuntu_original.package_target("lib/modules/other/kernel/driver.ko", release))
        for bad in ("../escape", "/host", "lib/../escape", "lib//escape"):
            with self.assertRaises(ValueError):
                ubuntu_original.member_name(bad)

    def test_ubuntu_original_input_rejects_links_and_digest_mismatch(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            path = directory / "input"
            path.write_bytes(b"locked input")
            digest = prepare.digest(path)
            with ubuntu_original.locked_input(path, digest, len(b"locked input")) as stream:
                self.assertEqual(stream.read(), b"locked input")
            with self.assertRaises(ValueError):
                ubuntu_original.locked_input(path, "f" * 64)
            link = directory / "link"
            link.symlink_to(path)
            with self.assertRaises(OSError):
                ubuntu_original.locked_input(link, digest)

    def test_ubuntu_original_recipe_keeps_locked_kernel_modules_and_generates_initrd(self):
        for architecture in ("x86_64", "aarch64"):
            with self.subTest(architecture=architecture), tempfile.TemporaryDirectory() as directory:
                directory = Path(directory)
                input_dir = directory / "input"
                input_dir.mkdir()
                for name in ("rootfs.tar.gz", "kernel", "config"):
                    (input_dir / name).write_bytes((name + " fixture").encode())
                root_sha = prepare.digest(input_dir / "rootfs.tar.gz")
                entry = {"rootfs_sha256": root_sha, "rootfs_tar_sha256": "a" * 64,
                         "runtime_kernel_sha256": prepare.digest(input_dir / "kernel"),
                         "config_sha256": prepare.digest(input_dir / "config")}
                release = "6.8.0-147-generic"
                (input_dir / "provenance.json").write_text(json.dumps({"architecture": architecture,
                    "rootfs_sha256": root_sha, "kernel_release": release}))
                data = json.dumps({"kernel_release": release, "architectures": {architecture: entry}}).encode()
                (directory / "ubuntu-original.lock.json").write_bytes(data)
                (directory / "lock.json").write_text(json.dumps({"families": {"ubuntu": {
                    "original_kernel_lock_sha256": hashlib.sha256(data).hexdigest()}}}))
                path = directory / "recipe.json"
                with mock.patch.object(ubuntu_render, "BASE", directory):
                    ubuntu_render.recipe(architecture, input_dir, directory / "out.fyimg", path)
                result = json.loads(path.read_text())
                self.assertEqual((result["kernel"]["origin"], result["kernel"]["modules"]), ("distribution-binary", "rootfs"))
                self.assertEqual(result["kernel"]["source"]["sha256"], entry["runtime_kernel_sha256"])
                self.assertEqual(result["generated_initrd_path"], "/boot/initrd.img-" + release)
                self.assertIn(["update-initramfs", "-c", "-k", release], [step["argv"] for step in result["steps"]])
                boot_args = result["boot"]["kernel_args"]
                self.assertEqual([arg for arg in boot_args if arg.startswith("console=")], ["console=ttyS0"])
                self.assertEqual("keep_bootcon" in boot_args, architecture == "aarch64")
                self.assertFalse(result["network"])
                (input_dir / "kernel").write_bytes(b"tampered")
                with mock.patch.object(ubuntu_render, "BASE", directory), self.assertRaises(ValueError):
                    ubuntu_render.recipe(architecture, input_dir, directory / "other.fyimg", directory / "other.json")
                self.assertFalse((directory / "other.json").exists())

    def test_normalization_removes_only_exact_locked_device_nodes(self):
        with tempfile.TemporaryDirectory() as directory:
            directory = Path(directory)
            source = directory / "root.tar.gz"
            with tarfile.open(source, "w:gz") as archive:
                device = tarfile.TarInfo("dev/null")
                device.type, device.devmajor, device.devminor = tarfile.CHRTYPE, 1, 3
                archive.addfile(device)
                content = b"guest configuration\n"
                regular = tarfile.TarInfo("etc/fixture")
                regular.mode, regular.uid, regular.gid, regular.size = 0o640, 12, 34, len(content)
                archive.addfile(regular, io.BytesIO(content))
                link = tarfile.TarInfo("etc/guest-link")
                link.type, link.linkname = tarfile.SYMTYPE, "/etc/fixture"
                archive.addfile(link)
                escaped = tarfile.TarInfo(r"usr/lib/systemd/system/system-systemd\x2dcryptsetup.slice")
                archive.addfile(escaped, io.BytesIO(b""))
            devices = [{"path": "dev/null", "major": 1, "minor": 3}]
            outputs = [directory / "first", directory / "second"]
            for output in outputs:
                self.assertEqual(prepare.normalize_rootfs(source, output, devices), ["dev/null"])
            self.assertEqual(outputs[0].read_bytes(), outputs[1].read_bytes())
            with tarfile.open(outputs[0], "r:gz") as archive:
                self.assertEqual(archive.getnames(), ["etc/fixture", "etc/guest-link", r"usr/lib/systemd/system/system-systemd\x2dcryptsetup.slice"])
                regular = archive.getmember("etc/fixture")
                self.assertEqual((regular.mode, regular.uid, regular.gid), (0o640, 12, 34))
                self.assertEqual(archive.extractfile(regular).read(), content)
                self.assertEqual(archive.getmember("etc/guest-link").linkname, "/etc/fixture")
            for index, devices in enumerate(([], [{"path": "dev/null", "major": 1, "minor": 5}],
                                             [{"path": "dev/zero", "major": 1, "minor": 5}])):
                with self.assertRaises(ValueError):
                    prepare.normalize_rootfs(source, directory / ("bad-" + str(index)), devices)

    def test_normalization_does_not_accept_unknown_special_or_escaping_members(self):
        for name, kind in (("../outside", tarfile.REGTYPE), ("dev/unknown", tarfile.CHRTYPE),
                           ("var/fifo", tarfile.FIFOTYPE)):
            with self.subTest(name=name), tempfile.TemporaryDirectory() as directory:
                directory = Path(directory)
                source = directory / "root.tar"
                with tarfile.open(source, "w") as archive:
                    info = tarfile.TarInfo(name)
                    info.type = kind
                    archive.addfile(info)
                with self.assertRaises(ValueError):
                    prepare.normalize_rootfs(source, directory / "normalized", [])


if __name__ == "__main__":
    unittest.main()
