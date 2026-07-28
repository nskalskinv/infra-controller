-- Add bmc_vendor_override to expected_machines so an operator can pin the
-- Redfish BMC vendor for a host. NULL means automatic detection. The value is a
-- RedfishVendor variant name, forced into libredfish when a client is built.
ALTER TABLE expected_machines ADD COLUMN bmc_vendor_override text;
