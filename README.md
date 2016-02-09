Here's a little IMAP-client lib in Go.

* It contains the Go "imap" package that implements the IMAP client protocol 
* This has only been officially tested against gmail's IMAP, but judging from the commit log, it should also work for Yahoo mail etc as well.
* The "upstream" changes will be monitored and tracked, and applied here.
* The sample executable originally provided has been stripped from this library, so as to provide a slim library for clean use.
  - For the original `imapsync` simple demo that uses the "imap" library to list or download gmail labels (= inboxes), check out [taliesinb's original implementation](https://github.com/taliesinb/go-imap/tree/master/src/imapsync) if you don't like the idea of specifying your username/password on the command line each time.
  - For a more complete implementation that is capable to sync your online and off-line mails repeatedly without re-downloading the old messages, or to sync two mail folders from two different cloud mail accounts into a single mail box file, check out [cloudmail](https://github.com/suntong/cloudmail/).
  - Just FTR, to send each message from within the mail box file as separated email to another (cloud) mail accounts, check out `formail` from the `procmail` package. I.e., you don't need to pay $$$ to sync messages between your different cloud mail accounts.
