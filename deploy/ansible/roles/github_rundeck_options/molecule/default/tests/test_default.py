import json
import pytest


def test_service_user_and_group(host):
    user = host.user("rundeck-gh")
    assert user.exists
    assert user.group == "rundeck-gh"
    assert user.shell in ("/sbin/nologin", "/usr/sbin/nologin")


def test_binary_installed(host):
    binary = host.file("/usr/local/bin/github-rundeck-options")
    assert binary.exists
    assert binary.is_file
    assert binary.user == "root"
    assert binary.mode == 0o755


def test_config_permissions_and_content(host):
    cfg_dir = host.file("/etc/github-rundeck-options")
    assert cfg_dir.exists
    assert cfg_dir.is_directory
    assert cfg_dir.user == "root"
    assert cfg_dir.group == "rundeck-gh"
    assert cfg_dir.mode == 0o750

    cfg_file = host.file("/etc/github-rundeck-options/config.json")
    assert cfg_file.exists
    assert cfg_file.is_file
    assert cfg_file.user == "root"
    assert cfg_file.group == "rundeck-gh"
    assert cfg_file.mode == 0o640

    parsed = json.loads(cfg_file.content_string)
    assert parsed["github_org"] == "molecule-test-org"
    assert parsed["cache_ttl_seconds"] == 600


def test_systemd_service_running_and_enabled(host):
    svc = host.service("github-rundeck-options")
    assert svc.is_enabled
    assert svc.is_running


@pytest.mark.parametrize("endpoint,expected_substring", [
    ("http://127.0.0.1:8080/health", '"status"'),
    ("http://127.0.0.1:8080/metrics", "github_cache_members_total"),
])
def test_http_endpoints_respond(host, endpoint, expected_substring):
    cmd = host.run(f"curl -fsS {endpoint}")
    assert cmd.rc == 0
    assert expected_substring in cmd.stdout
