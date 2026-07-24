#!/usr/bin/env python2.7
# Rewrite an upstream CalDAVTester serverinfo.xml in place so it targets a
# calserver instance (sabre URL layout) instead of Apple Calendar Server.
#
#   python2.7 serverinfo-overrides.py <path-to-serverinfo.xml> [host] [port]
#
# Changes: host/port, a trimmed <features> list (only what calserver
# implements), and the substitution building blocks that map CalDAVTester's
# __uids__/GUID paths onto sabre's /calendars/<user>/ layout.
import re
import sys

path = sys.argv[1]
host = sys.argv[2] if len(sys.argv) > 2 else "127.0.0.1"
port = sys.argv[3] if len(sys.argv) > 3 else "8008"

s = open(path).read()

# Host / port (nonssl). CalDAVTester adds $host:/$hostssl: from these.
s = re.sub(r"<host>.*?</host>", "<host>%s</host>" % host, s, count=1)
s = re.sub(r"<nonsslport>.*?</nonsslport>",
           "<nonsslport>%s</nonsslport>" % port, s, count=1)

# Only the features calserver actually implements. This gates which test
# files (and sub-tests) run, so listing an unsupported feature just yields
# noise failures.
features = """	<features>
		<feature>COPY Method</feature>
		<feature>MOVE Method</feature>
		<feature>Extended MKCOL</feature>
		<feature>acl-principal-prop-set REPORT</feature>
		<feature>principal-match REPORT</feature>
		<feature>principal-property-search REPORT</feature>
		<feature>principal-search-property-set REPORT</feature>
		<feature>brief</feature>
		<feature>current-user-principal</feature>
		<feature>expand-property</feature>
		<feature>sync-report</feature>
		<feature>sync-report-home</feature>
		<feature>well-known</feature>
		<feature>json-data</feature>
		<feature>caldav</feature>
		<feature>carddav</feature>
	</features>"""
s = re.sub(r"\t<features>.*?</features>", features, s, count=1, flags=re.S)

# Substitution remaps: sabre exposes /principals/<user>/, /calendars/<user>/,
# /addressbooks/<user>/ (no __uids__/GUID indirection), and names the
# notifications collection "notifications".
remaps = [
    (r"(<key>\$principals_users:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principalcollection:\g<2>"),
    (r"(<key>\$calendarhome%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$userid%d:\g<2>"),
    (r"(<key>\$addressbookhome%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$addressbooks:$userid%d:\g<2>"),
    (r"(<key>\$notification:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>notifications\g<2>"),
    (r"(<key>\$email%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$userid%d:@example.org\g<2>"),
]
for pat, repl in remaps:
    s, n = re.subn(pat, repl, s, count=1)
    if n != 1:
        sys.stderr.write("warning: remap did not apply once: %s\n" % pat)

open(path, "w").write(s)
print("patched %s -> host=%s port=%s, %d features" %
      (path, host, port, s.count("<feature>")))
