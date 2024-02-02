# How to create a windows installer 

## Overview

This describes how to create an EXE-Installer for Windows of the
sql_exporter. 

You can use the sql_exporter to export metrics of an MSSQL-server. The
corresponding installer will copy the files to the correct program
directory (%PROGRAMFILES%\sql_exporter), register the program via NSSM
(see below) as a windows service and it will open the Windows Firewall
for the corresponding port (9237/tcp).

Additional uninstall information will be created.

You will need additional software to create the Installer-EXE.


## Quick steps

- Get NSIS from http://nsis.sourceforge.net/Main_Page
- Compile the sql_exporter to match the Windows platform (cross compile or compile native under windows)
- Get NSSM - the Non-Sucking Service Manager from http://nssm.cc/
- Compile the NSIS script 
- Use the resulting sql_exporter_setup.exe to deploy the sql_exporter to you Windows hosts.


## Long description

### Requirements

- NSIS from http://nsis.sourceforge.net/Main_Page
- Compile the sql_exporter to match the Windows platform (cross compile or compile native under windows)
- NSSM - the Non-Sucking Service Manager from http://nssm.cc/ (archiv)


### Directory structure

This layout is based on drive D:\. Adopt this configuration or change the corresponding pathes in the NSIS script.

Path / File | Description
------------|------------
d:\sql_exporter\sql_exporter.exe | binary of the exporter
d:\sql_exporter\config.yml | example configuration (take a look at examples/config/mssql_config.yml)
d:\sql_exporter\source | place the source code of the used sql_exporter here
d:\sql_exporter\supplementals\sql_exporter.nsi | NSIS script for create the Installer-EXE


Extract NSSM to D:\sql_exporter\supplementals\nssm:

Path / File | Description | External source
------------|-------------|---------------- 
d:\sql_exporter\supplementals\nssm | extract NSSM to this directory | NSSM
d:\sql_exporter\supplementals\nssm\source | Source code of NSSM | NSSM
d:\sql_exporter\supplementals\nssm\win32\ | NSSM 32 bit | NSSM
d:\sql_exporter\supplementals\nssm\win64\ | NSSM 64 bit | NSSM


### Create the Installer

Just right click on D:\sql_exporter\supplementals\sql_exporter.nsi and
chose "Compile NSIS script" after installing NSIS. The script will
create the file D:\sql_exporter\supplementals\sql_exporter_setup.exe.
