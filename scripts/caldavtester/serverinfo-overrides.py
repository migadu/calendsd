#!/usr/bin/env python2.7
# Rewrite an upstream CalDAVTester serverinfo.xml in place so it targets a
# calserver instance (sabre URL layout) instead of Apple Calendar Server.
#
#   python2.7 serverinfo-overrides.py [--full] <path-to-serverinfo.xml> [host] [port]
#
# Changes: host/port and the substitution building blocks that map
# CalDAVTester's __uids__/GUID paths onto sabre's /calendars/<user>/ layout.
# By default the <features> list is trimmed to what calserver implements, so
# only applicable files run (the curated harness). With --full the upstream
# feature list is kept and the wait loop is shortened, so the ENTIRE suite runs
# and you see the raw pass/skip/fail spread (the full sweep).
import re
import sys

argv = [a for a in sys.argv[1:] if a != "--full"]
full = "--full" in sys.argv
path = argv[0]
host = argv[1] if len(argv) > 1 else "127.0.0.1"
port = argv[2] if len(argv) > 2 else "8008"

s = open(path).read()

# Host / port (nonssl). CalDAVTester adds $host:/$hostssl: from these.
s = re.sub(r"<host>.*?</host>", "<host>%s</host>" % host, s, count=1)
s = re.sub(r"<nonsslport>.*?</nonsslport>",
           "<nonsslport>%s</nonsslport>" % port, s, count=1)

if full:
    # Keep every feature so all files execute; shorten the wait loop so
    # missing-resource polls fail fast instead of stalling the sweep.
    s = re.sub(r"<waitcount>.*?</waitcount>", "<waitcount>5</waitcount>", s, count=1)
    s = re.sub(r"<waitdelay>.*?</waitdelay>", "<waitdelay>0.1</waitdelay>", s, count=1)

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
		<feature>well-known</feature>
		<feature>json-data</feature>
		<feature>caldav</feature>
		<feature>carddav</feature>
	</features>"""
if not full:
    s = re.sub(r"\t<features>.*?</features>", features, s, count=1, flags=re.S)

# Substitution remaps: sabre exposes /principals/<user>/, /calendars/<user>/,
# /addressbooks/<user>/ (no __uids__/GUID indirection), and names the
# notifications collection "notifications".
remaps = [
    (r"(<key>\$principals_users:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principalcollection:\g<2>"),
    # calserver exposes principals as /principals/<user>/, not the Calendar
    # Server __uids__/GUID form, so current-user-principal and expand-property
    # compare against the users path.
    (r"(<key>\$principaluri%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principals_users:$userid%d:/\g<2>"),
    (r"(<key>\$calendarhome%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$userid%d:\g<2>"),
    (r"(<key>\$addressbookhome%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$addressbooks:$userid%d:\g<2>"),
    (r"(<key>\$notification:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>notifications\g<2>"),
    (r"(<key>\$email%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$userid%d:@example.org\g<2>"),
    # Resource / location / group principals live flat under /principals/
    # (seeded as resourceNN/locationNN/groupNN by seed.sql), with calendars
    # under /calendars/<name>/ and mailto: calendar-user addresses instead of
    # CS's urn:x-uid GUID form.
    (r"(<key>\$principals_resources:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principalcollection:\g<2>"),
    (r"(<key>\$principals_groups:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principalcollection:\g<2>"),
    (r"(<key>\$rprincipaluri%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principalcollection:$resourceid%d:/\g<2>"),
    (r"(<key>\$rcalendarhome%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$resourceid%d:\g<2>"),
    (r"(<key>\$rcalendarpath%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$resourceid%d:/$calendar:\g<2>"),
    (r"(<key>\$rinboxpath%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$resourceid%d:/$inbox:\g<2>"),
    (r"(<key>\$routboxpath%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$resourceid%d:/$outbox:\g<2>"),
    (r"(<key>\$rcuaddrurn%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>mailto:$resourceid%d:@example.org\g<2>"),
    (r"(<key>\$lprincipaluri%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principalcollection:$locationid%d:/\g<2>"),
    (r"(<key>\$lcalendarhome%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$locationid%d:\g<2>"),
    (r"(<key>\$lcalendarpath%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$locationid%d:/$calendar:\g<2>"),
    (r"(<key>\$linboxpath%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$locationid%d:/$inbox:\g<2>"),
    (r"(<key>\$loutboxpath%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$calendars:$locationid%d:/$outbox:\g<2>"),
    (r"(<key>\$lcuaddrurn%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>mailto:$locationid%d:@example.org\g<2>"),
    (r"(<key>\$gprincipaluri%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>$principalcollection:$groupid%d:/\g<2>"),
    (r"(<key>\$gcuaddrurn%d:</key>\s*<value>)[^<]*(</value>)",
     r"\g<1>mailto:$groupid%d:@example.org\g<2>"),
]
for pat, repl in remaps:
    s, n = re.subn(pat, repl, s, count=1)
    if n != 1:
        sys.stderr.write("warning: remap did not apply once: %s\n" % pat)

open(path, "w").write(s)
print("patched %s -> host=%s port=%s, %d features (%s)" %
      (path, host, port, s.count("<feature>"), "full" if full else "curated"))
