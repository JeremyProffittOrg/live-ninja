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
