import json
import unittest

from package_installers import pack_record


class PackRecordTests(unittest.TestCase):
    def test_npm_11_and_12(self):
        info = {"name": "@brokkai/review-bot", "version": "0.2.3",
                "integrity": "sha512-fixture", "filename": "review-bot.tgz"}
        for output in ([info], {info["name"]: info}):
            self.assertEqual(pack_record(json.dumps(output), info["name"], "0.2.3"), info)

    def test_rejects_wrong_package_version_and_path(self):
        info = {"name": "@brokkai/review-bot", "version": "0.2.3",
                "integrity": "sha512-fixture", "filename": "review-bot.tgz"}
        for fields in ({"name": "other"}, {"version": "0.2.2"},
                       {"filename": "../wrong.tgz"}, {"integrity": ""}):
            with self.assertRaises(ValueError):
                pack_record(json.dumps([dict(info, **fields)]), info["name"], "0.2.3")
        for output in ({}, [], [info, info]):
            with self.assertRaises(ValueError):
                pack_record(json.dumps(output), info["name"], "0.2.3")
