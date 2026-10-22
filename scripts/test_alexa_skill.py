"""Offline checks for the Alexa delivery/credential helpers; no account calls."""
import importlib.util
from pathlib import Path
import unittest
import http.server
import threading
import urllib.error
import urllib.request
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("alexa_setup", Path(__file__).with_name("setup_alexa_credentials.py"))
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)

delivery_spec = importlib.util.spec_from_file_location("alexa_delivery", Path(__file__).with_name("alexa-skill.py"))
delivery = importlib.util.module_from_spec(delivery_spec)
delivery_spec.loader.exec_module(delivery)


class DeliveryTests(unittest.TestCase):
    def test_cli_failure_reports_code_without_provider_payload(self):
        import subprocess
        failed = subprocess.CompletedProcess([], 255, "", "An error occurred (AccessDeniedException) when calling GetParameters: private-payload-must-not-appear")
        with patch.object(delivery.subprocess, "run", return_value=failed):
            with self.assertRaises(delivery.Failure) as caught:
                delivery.command(["aws", "ssm", "get-parameters"])
        self.assertIn("AccessDeniedException", str(caught.exception))
        self.assertNotIn("private-payload", str(caught.exception))

    def test_trial_refuses_unreported_usage(self):
        with patch.object(delivery, "aws", return_value={"Items": [{"reported": {"BOOL": False}}]}):
            with self.assertRaises(delivery.Failure):
                delivery.trial_usage("owner", "table")

    def test_simulation_fixture_contains_fifty_bounded_cases(self):
        import json
        cases=json.loads((delivery.ROOT/"alexa/simulations.json").read_text())
        self.assertEqual(50,len(cases))
        self.assertTrue(all(len(c["utterance"]) < 150 for c in cases))
        self.assertIn("No I meant West Virginia",[c["utterance"] for c in cases])

    def test_package_uses_real_endpoint_and_dialog(self):
        import io
        import json
        import zipfile
        manifest, model = delivery.local_files("arn:aws:lambda:us-east-1:759775734231:function:live-ninja-alexa-skill")
        with zipfile.ZipFile(io.BytesIO(delivery.package(manifest, model))) as archive:
            self.assertEqual({"skill.json", "interactionModels/custom/en-US.json"}, set(archive.namelist()))
            data = json.loads(archive.read("interactionModels/custom/en-US.json"))
            self.assertTrue(data["interactionModel"]["dialog"]["intents"][0]["slots"][0]["elicitationRequired"])
        with self.assertRaises(delivery.Failure):
            delivery.local_files("arn:aws:lambda:us-east-1:111111111111:function:live-ninja-alexa-skill")

    def test_uncertain_create_cannot_create_another_skill(self):
        class EmptyAPI:
            def call(self, path): return {"skills": [], "isTruncated": False}, {}
        with self.assertRaises(delivery.Failure):
            delivery.reconcile_skill(EmptyAPI(), {"createAttempted": True}, "endpoint")

    def test_import_failure_is_terminal(self):
        class FailedAPI:
            def call(self, path): return {"status": "FAILED"}, {}
        with self.assertRaises(delivery.Failure), patch.object(delivery.time, "sleep") as sleep:
            delivery.poll_import(FailedAPI(), "/v1/skills/imports/test-id")
        sleep.assert_not_called()

    def test_redirects_bind_the_verified_vendor(self):
        redirects = delivery.redirect_uris(delivery.VENDOR)
        self.assertEqual(3, len(redirects))
        self.assertTrue(all(r.endswith("/api/skill/link/M3M80BVXTGPEZW") for r in redirects))
        with self.assertRaises(delivery.Failure):
            delivery.redirect_uris("unverified")

    def test_simulation_does_not_save_tokens_or_pass_account_link_prompt(self):
        body = {"id": "simulation", "status": "SUCCESSFUL", "result": {"skillExecutionInfo": {"invocations": [{
            "invocationRequest": {"body": {"context": {"System": {"user": {"accessToken": "never-save-this"}}}, "request": {"type": "IntentRequest", "requestId": "request-id", "intent": {"name": "ChatIntent", "slots": {"query": {"value": "two plus two"}}}}}},
            "invocationResponse": {"body": {"response": {"card": {"type": "LinkAccount"}, "outputSpeech": {"text": "Link your account"}}}},
            "metrics": {"skillExecutionTimeInMilliseconds": 250}}]}}}
        import json
        record = delivery.simulation_record(body, {"expected": "chat"})
        self.assertNotIn("never-save-this", json.dumps(record))
        self.assertFalse(record["passed"])
        self.assertEqual("account_link_required", record["outcome"])

    def test_acceptance_requires_physical_evidence(self):
        with self.assertRaises(delivery.Failure):
            delivery.validate_acceptance({"passed": True}, {}, {})


class CredentialEntryTests(unittest.TestCase):
    def setUp(self):
        printer=patch("builtins.print")
        printer.start()
        self.addCleanup(printer.stop)

    def test_root_entry_redirects_instead_of_claiming_authorization_failed(self):
        real_server=http.server.HTTPServer
        observations=[]
        threads=[]

        class NoRedirect(urllib.request.HTTPRedirectHandler):
            def redirect_request(self, req, fp, code, msg, headers, newurl):
                return None

        def make_server(address, handler):
            server=real_server(("127.0.0.1",0),handler)
            def probe():
                opener=urllib.request.build_opener(NoRedirect())
                for path in ("/", "/cb?state=synthetic-state&error=access_denied"):
                    try:
                        response=opener.open("http://127.0.0.1:"+str(server.server_port)+path,timeout=3)
                    except urllib.error.HTTPError as error:
                        response=error
                    with response:
                        observations.append((response.code,response.read().decode()))
            thread=threading.Thread(target=probe,daemon=True);threads.append(thread);thread.start()
            return server

        with patch.object(setup.secrets,"token_urlsafe",return_value="synthetic-state"), patch.object(setup.http.server,"HTTPServer",side_effect=make_server):
            with self.assertRaises(RuntimeError):
                setup.authorize("synthetic-client","synthetic-secret")
        for thread in threads:
            thread.join(timeout=3)
            self.assertFalse(thread.is_alive())
        self.assertEqual(302,observations[0][0])
        self.assertNotIn("Authorization failed",observations[0][1])

    def test_empty_input_does_not_write_or_delete(self):
        with patch.object(setup, "command") as run:
            setup.write_value("secret", "ALEXA_SMAPI_REFRESH_TOKEN", "")
            run.assert_not_called()

    def test_secret_metacharacters_use_stdin_and_exact_repo(self):
        value = 'test&value|$(do-not-run)"'
        with patch.object(setup, "command", side_effect=["", '[{"name":"ALEXA_SMAPI_CLIENT_SECRET"}]']) as run:
            setup.write_value("secret", "ALEXA_SMAPI_CLIENT_SECRET", value)
            self.assertEqual(run.call_args_list[0].args, (["gh", "secret", "set", "ALEXA_SMAPI_CLIENT_SECRET", "--repo", "JeremyProffittOrg/live-ninja"], value))

    def test_missing_name_is_not_reported_as_verified(self):
        with patch.object(setup, "command", side_effect=["", "[]"]):
            with self.assertRaises(RuntimeError):
                setup.write_value("secret", "ALEXA_SMAPI_CLIENT_SECRET", "synthetic-secret")


if __name__ == "__main__":
    unittest.main()
