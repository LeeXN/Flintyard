import importlib.util
import os
from pathlib import Path
import shutil
import struct
import socket
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("egress_server", ROOT / "tests/kvm-egress-server.py")
server = importlib.util.module_from_spec(spec)
spec.loader.exec_module(server)
spoof_spec = importlib.util.spec_from_file_location("spoof_probe", ROOT / "tests/kvm-spoof-probe.py")
spoof = importlib.util.module_from_spec(spoof_spec)
spoof_spec.loader.exec_module(spoof)


class ControlledDNS(unittest.TestCase):
    def test_ubuntu_probe_invokes_non_executable_shared_probe_with_bash(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for name in ("kvm-guest-check.sh", "kvm-ubuntu-original-check.sh"):
                shutil.copyfile(ROOT / "tests" / name, root / name)
                (root / name).chmod(0o644)
            (root / "key").write_text("private test placeholder")
            (root / "known-hosts").write_text("host key placeholder")
            bindir = root / "bin"
            bindir.mkdir()
            for name, body in (("ssh-keygen", "exit 0\n"),
                               ("ssh", 'printf "call\\n" >> "$KVM_SSH_CALLS"\nexit 0\n')):
                path = bindir / name
                path.write_text("#!/bin/sh\n" + body)
                path.chmod(0o755)
            calls = root / "calls"
            env = dict(os.environ, PATH=str(bindir) + ":/usr/bin:/bin", KVM_SSH_CALLS=str(calls))
            result = subprocess.run(["bash", str(root / "kvm-ubuntu-original-check.sh"), "192.0.2.2",
                                     str(root / "key"), str(root / "known-hosts"), "6.8.0-test", "192.0.2.53"],
                                    env=env, text=True, capture_output=True, timeout=10)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(calls.read_text().splitlines(), ["call", "call"])

    def test_returns_only_the_test_name_and_a_record(self):
        header = struct.pack("!HHHHHH", 0x1234, 0x0100, 1, 0, 0, 0)
        name = b"\x0eflintyard-test\x07invalid\x00"
        response, answered = server.dns_response(header + name + struct.pack("!HH", 1, 1))
        self.assertTrue(answered)
        self.assertEqual(response[:2], b"\x12\x34")
        self.assertEqual(response[-4:], bytes((198, 51, 100, 42)))
        self.assertEqual(struct.unpack("!H", response[6:8])[0], 1)
        response, answered = server.dns_response(header + name + struct.pack("!HH", 28, 1))
        self.assertFalse(answered)
        self.assertEqual(struct.unpack("!H", response[6:8])[0], 0)
        for data in (b"short", header + b"\xc0\x0c" + struct.pack("!HH", 1, 1),
                     header + b"\x05other\x07invalid\x00" + struct.pack("!HH", 1, 1)):
            self.assertEqual(server.dns_response(data), (None, False))

    def test_spoof_probe_has_valid_ipv4_and_udp_checksums(self):
        frame = spoof.packet("02:00:00:00:00:01", "02:00:00:00:00:02", "192.0.2.1", 58902)
        self.assertEqual(frame[12:14], b"\x08\x00")
        header, udp = frame[14:34], frame[34:]
        self.assertEqual(socket.inet_ntoa(header[12:16]), "127.0.0.9")
        self.assertEqual(socket.inet_ntoa(header[16:20]), "192.0.2.1")
        self.assertEqual(spoof.checksum(header), 0)
        pseudo = header[12:20] + struct.pack("!BBH", 0, socket.IPPROTO_UDP, len(udp))
        self.assertEqual(spoof.checksum(pseudo + udp), 0)
        self.assertEqual(struct.unpack("!H", udp[2:4])[0], 58902)
