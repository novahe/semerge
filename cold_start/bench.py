#!/usr/bin/env python3
"""Create fresh img sandboxes, measure the first command, and split stages."""
import sys

from stages import main


if __name__ == '__main__':
    sys.exit(main())
