#!/bin/sh
# 双击或直接运行此脚本即可打开交互式数字菜单
cd "$(dirname "$0")"
PYTHONPATH=tools python3 tools/menu.py
