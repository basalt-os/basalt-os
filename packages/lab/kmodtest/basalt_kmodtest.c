// SPDX-License-Identifier: GPL-2.0
/*
 * Basalt OS lab fixture: a kernel module that does nothing but log a line
 * when it is loaded and unloaded. Built out of tree and loaded unsigned,
 * signed with a foreign key and signed with the Basalt module key, to show
 * which signatures the kernel accepts under Secure Boot and lockdown.
 */
#include <linux/init.h>
#include <linux/module.h>

static int __init basalt_kmodtest_init(void)
{
	pr_info("basalt_kmodtest: loaded\n");
	return 0;
}

static void __exit basalt_kmodtest_exit(void)
{
	pr_info("basalt_kmodtest: unloaded\n");
}

module_init(basalt_kmodtest_init);
module_exit(basalt_kmodtest_exit);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("Basalt OS lab: module signature test fixture");
