package ebs

import (
	"fmt"
	"github.com/mitchellh/goamz/ec2"
	"github.com/mitchellh/multistep"
	awscommon "github.com/mitchellh/packer/builder/amazon/common"
	"github.com/mitchellh/packer/packer"
	"database/sql"
	"github.com/mattn/go-sqlite3"
)

type stepCreateAMI struct {
	image *ec2.Image
}

func (s *stepCreateAMI) Run(state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(config)
	ec2conn := state.Get("ec2").(*ec2.EC2)
	image := state.Get("source_image").(*ec2.Image)
	instance := state.Get("instance").(*ec2.Instance)
	ui := state.Get("ui").(packer.Ui)

	// Create the image
	ui.Say(fmt.Sprintf("Creating the AMI: %s", config.AMIName))
	createOpts := &ec2.CreateImage{
		InstanceId:   instance.InstanceId,
		Name:         config.AMIName,
		BlockDevices: config.BlockDevices.BuildAMIDevices(),
	}

	createResp, err := ec2conn.CreateImage(createOpts)
	if err != nil {
		err := fmt.Errorf("Error creating AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	// Set the AMI ID in the state
	ui.Message(fmt.Sprintf("AMI: %s", createResp.ImageId))
	amis := make(map[string]string)
	amis[ec2conn.Region.Name] = createResp.ImageId
	state.Put("amis", amis)

	// Wait for the image to become ready
	stateChange := awscommon.StateChangeConf{
		Pending:   []string{"pending"},
		Target:    "available",
		Refresh:   awscommon.AMIStateRefreshFunc(ec2conn, createResp.ImageId),
		StepState: state,
	}

	ui.Say("ebs - Waiting for AMI to become ready...")
	if _, err := awscommon.WaitForState(&stateChange); err != nil {
		err := fmt.Errorf("Error waiting for AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	var ami_type string
	if "paravirtual" == image.VirtualizationType {
		ami_type = "pv"
	} else {
		ami_type = "hvm"
	}

	// AMI is ready. Update the database.
	ui.Say("AMI created. Updating the database now.")
	var DB_DRIVER string
	sql.Register(DB_DRIVER, &sqlite3.SQLiteDriver{})
	db, err := sql.Open(DB_DRIVER, "pacman.db")
	checkErr(err, state, "failed to create the database handle")

	stmt, err := db.Prepare("update bake_ami set ami_status=1, ami_id=? where region=? and ami_type=?")
	checkErr(err, state, "preparing update query failed")

	res, err := stmt.Exec(createResp.ImageId, ec2conn.Region.Name, ami_type)
	checkErr(err, state, "update execution failed")

	affect, err := res.RowsAffected()
	checkErr(err, state, "update db failed")

	ui.Say(fmt.Sprintf("Updated database with %d row(s) affected", affect))
	db.Close()

	imagesResp, err := ec2conn.Images([]string{createResp.ImageId}, nil)
	if err != nil {
		err := fmt.Errorf("Error searching for AMI: %s", err)
		state.Put("error", err)
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	s.image = &imagesResp.Images[0]

	return multistep.ActionContinue
}

func checkErr(err error, state multistep.StateBag, msg string) {
	if err != nil {
	  err := fmt.Errorf(msg)
	  state.Put("error", err)
	  ui := state.Get("ui").(packer.Ui)
	  ui.Error(err.Error())
	}
}

func (s *stepCreateAMI) Cleanup(state multistep.StateBag) {
	if s.image == nil {
		return
	}

	_, cancelled := state.GetOk(multistep.StateCancelled)
	_, halted := state.GetOk(multistep.StateHalted)
	if !cancelled && !halted {
		return
	}

	ec2conn := state.Get("ec2").(*ec2.EC2)
	ui := state.Get("ui").(packer.Ui)

	ui.Say("Deregistering the AMI because cancelation or error...")
	if resp, err := ec2conn.DeregisterImage(s.image.Id); err != nil {
		ui.Error(fmt.Sprintf("Error deregistering AMI, may still be around: %s", err))
		return
	} else if resp.Return == false {
		ui.Error(fmt.Sprintf("Error deregistering AMI, may still be around: %t", resp.Return))
		return
	}
}
